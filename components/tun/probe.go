/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for the v6 reachability probe. The target is the
// well-known Cloudflare DNS IPv6 anycast — stable, low load, and
// universally reachable from any host that has working public v6.
const (
	defaultIPv6ProbeTarget   = "[2606:4700:4700::1111]:443"
	defaultIPv6ProbeInterval = 5 * time.Minute
	defaultIPv6ProbeTimeout  = 4 * time.Second
)

// ipv6Prober periodically tests whether the snell server can reach
// public IPv6 destinations by dialing a known v6 target through the
// snell tunnel. The latest result is exposed via Allowed() so the
// TUN handler can synchronously decide whether to accept a real-v6
// destination or reject it (forcing happy-eyeballs to fall back to
// v4).
//
// Lifecycle: New → Start(ctx) → (handler calls Allowed any time) →
// Close. Stop is implied by ctx cancellation.
//
// The first probe runs synchronously inside Start so that Allowed
// returns a meaningful value before the TUN data path opens. If the
// first probe is slow (network hiccup), the data path still comes up
// but treats v6 as unreachable until the next periodic probe.
type ipv6Prober struct {
	dialer     Dialer
	targetHost string
	targetPort uint16
	interval   time.Duration
	timeout    time.Duration
	log        *slog.Logger

	allowed atomic.Bool

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// newIPv6Prober prepares a prober but does not run anything yet.
// target is "host:port" (v6 host may or may not be bracketed —
// net.SplitHostPort handles both).
func newIPv6Prober(dialer Dialer, target string, interval time.Duration, log *slog.Logger) (*ipv6Prober, error) {
	if target == "" {
		target = defaultIPv6ProbeTarget
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = defaultIPv6ProbeInterval
	}
	return &ipv6Prober{
		dialer:     dialer,
		targetHost: host,
		targetPort: uint16(port64),
		interval:   interval,
		timeout:    defaultIPv6ProbeTimeout,
		log:        log,
	}, nil
}

// Start runs the first probe synchronously, then a periodic refresher
// in the background until Close or ctx cancellation.
func (p *ipv6Prober) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.runOnce(ctx)

	loopCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				p.runOnce(loopCtx)
			}
		}
	}()
}

func (p *ipv6Prober) Close() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// Allowed reports whether the most recent probe succeeded. A nil
// prober (used to mean "feature disabled by config") always returns
// false.
func (p *ipv6Prober) Allowed() bool {
	if p == nil {
		return false
	}
	return p.allowed.Load()
}

// runOnce dials the probe target through the snell tunnel. Success
// (any non-error return from DialTCP within the probe timeout) is
// treated as "server has working v6". The connection is closed
// immediately — we only care that the SYN→SYN-ACK round-trip
// succeeded on the server's v6 path.
func (p *ipv6Prober) runOnce(ctx context.Context) {
	dialCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := p.dialer.DialTCP(dialCtx, p.targetHost, p.targetPort)
	prev := p.allowed.Load()
	if err != nil {
		p.allowed.Store(false)
		if prev {
			p.log.Info("ipv6 probe failed, disabling v6 forwarding",
				"target", net.JoinHostPort(p.targetHost, strconv.Itoa(int(p.targetPort))),
				"err", err)
		} else {
			p.log.Debug("ipv6 probe still failing", "err", err)
		}
		return
	}
	_ = conn.Close()
	p.allowed.Store(true)
	if !prev {
		p.log.Info("ipv6 probe succeeded, enabling v6 forwarding",
			"target", net.JoinHostPort(p.targetHost, strconv.Itoa(int(p.targetPort))))
	} else {
		p.log.Debug("ipv6 probe ok")
	}
}
