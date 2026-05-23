/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build linux

package tun

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	singtun "github.com/sagernet/sing-tun"
	"go4.org/netipx"
)

// linuxBypass mutates the IPSet that sing-tun's AutoRedirect uses for
// its RouteExcludeAddressSet, then asks AutoRedirect to re-publish
// the nftables rules on every change. Concurrent-safe.
//
// Wiring contract:
//
//  1. Construct with newLinuxBypass.
//  2. Add the fake-IP CIDR and any static DirectIPs via AddCIDR.
//  3. Pass routeSetPointer() into AutoRedirectOptions.RouteExcludeAddressSet.
//  4. After sing-tun.NewAutoRedirect succeeds, call attach(redirect, ctx).
//
// Steps 2 and 3 happen before AutoRedirect.Start so nftables comes up
// with the static prefixes already excluded. Step 4 enables the
// dynamic update path.
type linuxBypass struct {
	log *slog.Logger

	mu          sync.Mutex
	staticCIDRs []netip.Prefix
	dynamicIPs  map[netip.Addr]time.Time

	// routeSet is the slice sing-tun reads each Update. Its address
	// (returned from routeSetPointer) is stored in AutoRedirectOptions
	// at construction time; we reassign the field to publish changes.
	routeSet []*netipx.IPSet

	redirect singtun.AutoRedirect

	reaperOnce   sync.Once
	reaperCancel context.CancelFunc
	reaperWG     sync.WaitGroup
}

// reaperInterval is how often we sweep expired dynamic entries.
// Short enough that a TTL=60s DNS answer doesn't keep the IP in the
// bypass set for long after expiry, long enough that we don't spam
// nftables updates on idle traffic.
const reaperInterval = 30 * time.Second

func newLinuxBypass(log *slog.Logger) *linuxBypass {
	return &linuxBypass{
		log:        log,
		dynamicIPs: make(map[netip.Addr]time.Time),
		routeSet:   []*netipx.IPSet{emptyIPSet()},
	}
}

// routeSetPointer is the address that sing-tun's AutoRedirect should
// hold so its later Update calls see whatever the current contents
// of b.routeSet are. Must be called once, before NewAutoRedirect.
func (b *linuxBypass) routeSetPointer() *[]*netipx.IPSet {
	return &b.routeSet
}

// attach binds the AutoRedirect for dynamic updates and starts the
// TTL reaper. Must be called after sing-tun.NewAutoRedirect returns.
func (b *linuxBypass) attach(ctx context.Context, redirect singtun.AutoRedirect) {
	b.mu.Lock()
	b.redirect = redirect
	b.mu.Unlock()

	reaperCtx, cancel := context.WithCancel(ctx)
	b.reaperCancel = cancel
	b.reaperWG.Add(1)
	go b.reaperLoop(reaperCtx)
}

func (b *linuxBypass) AddCIDR(p netip.Prefix) error {
	if !p.IsValid() {
		return nil
	}
	b.mu.Lock()
	b.staticCIDRs = append(b.staticCIDRs, p)
	b.rebuildLocked()
	b.mu.Unlock()
	b.publish()
	return nil
}

func (b *linuxBypass) AddIP(ip netip.Addr, ttl time.Duration) error {
	if !ip.IsValid() {
		return nil
	}
	ip = ip.Unmap()
	if ttl <= 0 {
		// Permanent: promote to a /32 or /128 static CIDR. Keeps the
		// reaper from scanning entries that never go away.
		bits := 32
		if ip.Is6() {
			bits = 128
		}
		return b.AddCIDR(netip.PrefixFrom(ip, bits))
	}
	b.mu.Lock()
	newExpiry := time.Now().Add(ttl)
	if prev, ok := b.dynamicIPs[ip]; ok && prev.After(newExpiry) {
		// Existing entry's expiry is later — keep it, no rebuild.
		b.mu.Unlock()
		return nil
	}
	_, existed := b.dynamicIPs[ip]
	b.dynamicIPs[ip] = newExpiry
	if !existed {
		b.rebuildLocked()
	}
	b.mu.Unlock()
	if !existed {
		b.publish()
	}
	return nil
}

func (b *linuxBypass) Close() error {
	b.reaperOnce.Do(func() {
		if b.reaperCancel != nil {
			b.reaperCancel()
		}
		b.reaperWG.Wait()
	})
	return nil
}

// rebuildLocked recomputes the IPSet from staticCIDRs + non-expired
// dynamicIPs and swaps it into b.routeSet. Caller must hold b.mu.
func (b *linuxBypass) rebuildLocked() {
	builder := netipx.IPSetBuilder{}
	for _, p := range b.staticCIDRs {
		builder.AddPrefix(p)
	}
	now := time.Now()
	for ip, exp := range b.dynamicIPs {
		if exp.Before(now) {
			continue
		}
		bits := 32
		if ip.Is6() {
			bits = 128
		}
		builder.AddPrefix(netip.PrefixFrom(ip, bits))
	}
	set, err := builder.IPSet()
	if err != nil {
		// IPSetBuilder errors are limited to overlapping prefixes,
		// which can't happen here. Log and keep the previous set.
		b.log.Warn("bypass: ipset build", "err", err)
		return
	}
	b.routeSet = []*netipx.IPSet{set}
}

// publish asks sing-tun to re-read the route exclude set and update
// nftables. Calls into netlink — never hold b.mu across this.
func (b *linuxBypass) publish() {
	b.mu.Lock()
	r := b.redirect
	b.mu.Unlock()
	if r == nil {
		// Not attached yet — sing-tun will pick up the current
		// contents at its own Start time.
		return
	}
	r.UpdateRouteAddressSet()
}

// reaperLoop sweeps expired dynamic entries. On any actual eviction
// it rebuilds the IPSet and re-publishes.
func (b *linuxBypass) reaperLoop(ctx context.Context) {
	defer b.reaperWG.Done()
	t := time.NewTicker(reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if b.sweepExpired() {
				b.publish()
			}
		}
	}
}

// sweepExpired removes expired entries from dynamicIPs and returns
// whether anything was removed (i.e. whether a republish is needed).
func (b *linuxBypass) sweepExpired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	removed := false
	for ip, exp := range b.dynamicIPs {
		if exp.Before(now) {
			delete(b.dynamicIPs, ip)
			removed = true
		}
	}
	if removed {
		b.rebuildLocked()
	}
	return removed
}

// emptyIPSet returns the well-known empty IPSet so the initial
// routeSet slice element is never nil. sing-tun's matcher tolerates a
// nil/empty set, but having an explicit empty value keeps the
// invariant simple: routeSet always has exactly one element.
func emptyIPSet() *netipx.IPSet {
	set, _ := (&netipx.IPSetBuilder{}).IPSet()
	return set
}
