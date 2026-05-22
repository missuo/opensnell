/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build linux

package tun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	singtun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/ranges"

	"go4.org/netipx"

	"github.com/missuo/opensnell/components/utils"
)

// New brings up the TUN inbound. Lifecycle:
//
//   1. Create a kernel TUN device, address fake-IP-gateway/prefix-bits.
//   2. Run a userspace "system" stack on top of the TUN so packets
//      destined to fake-IPs land in our Handler.
//   3. Start an in-process UDP DNS server on the TUN gateway address,
//      answering A queries with fake-IPs from a bounded LRU pool.
//   4. Install sing-tun's auto-redirect nftables rules to capture all
//      *other* outbound TCP and DNAT it into a userspace TCP listener
//      (shared with our Handler). The fake-IP CIDR is excluded from
//      this redirect so fake-IP TCP stays on the TUN path above.
//   5. sing-tun's DNS hijack DNATs every UDP/TCP :53 outbound to our
//      DNS server's address, so apps that talk to any configured
//      resolver (1.1.1.1, the box's poisoned ISP DNS, anything) end up
//      asking us instead.
//
// Cleanup on Close reverses each step in opposite order and is
// idempotent (sing-tun internally re-cleans on next Start, so a
// SIGKILL-restart still recovers to a sane state).
func New(ctx context.Context, cfg Config, dialer Dialer, log *slog.Logger) (Inbound, error) {
	if log == nil {
		log = slog.Default()
	}
	if dialer == nil {
		return nil, errors.New("snell-tun: dialer is required")
	}
	if cfg.OutputMark == 0 {
		cfg.OutputMark = DefaultOutputMark
	}
	if cfg.MTU == 0 {
		cfg.MTU = 9000
	}
	if cfg.TUNName == "" {
		cfg.TUNName = "snell0"
	}
	if !cfg.FakeIPPrefix.IsValid() {
		cfg.FakeIPPrefix = DefaultFakeIPPrefix
	}
	if !cfg.FakeIPPrefix.Addr().Is4() {
		return nil, fmt.Errorf("snell-tun: fake-ip prefix %q must be IPv4", cfg.FakeIPPrefix)
	}

	pool, err := NewFakePool(cfg.FakeIPPrefix, 0)
	if err != nil {
		return nil, fmt.Errorf("snell-tun: build fake-ip pool: %w", err)
	}
	gateway := pool.Gateway()
	log.Info("snell tun fake-ip pool ready",
		"prefix", cfg.FakeIPPrefix.String(),
		"gateway", gateway.String(),
	)

	singLog := newSlogAdapter(log)

	finder := control.NewDefaultInterfaceFinder()
	netMon, err := singtun.NewNetworkUpdateMonitor(singLog)
	if err != nil {
		return nil, fmt.Errorf("snell-tun: network monitor: %w", err)
	}
	if err := netMon.Start(); err != nil {
		return nil, fmt.Errorf("snell-tun: start network monitor: %w", err)
	}
	ifMon, err := singtun.NewDefaultInterfaceMonitor(netMon, singLog, singtun.DefaultInterfaceMonitorOptions{
		InterfaceFinder: finder,
	})
	if err != nil {
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: default-interface monitor: %w", err)
	}
	if err := ifMon.Start(); err != nil {
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start default-interface monitor: %w", err)
	}

	excludeUIDs := make([]ranges.Range[uint32], 0, len(cfg.ExcludeUIDs))
	for _, uid := range cfg.ExcludeUIDs {
		excludeUIDs = append(excludeUIDs, ranges.NewSingle(uid))
	}

	// TUN address: assign the gateway. e.g. 198.18.128.1 with the
	// caller's prefix bits, so the kernel auto-routes the whole CIDR
	// over the TUN.
	tunPrefix := netip.PrefixFrom(gateway, cfg.FakeIPPrefix.Bits())

	tunOpts := &singtun.Options{
		Name:                   cfg.TUNName,
		Inet4Address:           []netip.Prefix{tunPrefix},
		MTU:                    cfg.MTU,
		AutoRedirectMarkMode:   true,
		AutoRedirectOutputMark: cfg.OutputMark,
		ExcludeUID:             excludeUIDs,
		// Hijack UDP/TCP :53 from apps and DNAT to our DNS server
		// listening on the TUN gateway. (TCP hijack lands on a port we
		// don't listen on — apps fall back to UDP, which is the common
		// case.)
		DNSServers:       []netip.Addr{gateway},
		InterfaceFinder:  finder,
		InterfaceMonitor: ifMon,
		Logger:           singLog,
	}

	device, err := singtun.New(*tunOpts)
	if err != nil {
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: create TUN device: %w", err)
	}

	h := &handler{ctx: ctx, dialer: dialer, log: log, pool: pool}

	stack, err := singtun.NewStack("system", singtun.StackOptions{
		Context:         ctx,
		Tun:             device,
		TunOptions:      *tunOpts,
		UDPTimeout:      5 * time.Minute,
		Handler:         h,
		Logger:          singLog,
		InterfaceFinder: finder,
	})
	if err != nil {
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: create stack: %w", err)
	}

	if err := stack.Start(); err != nil {
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start stack: %w", err)
	}
	if err := device.Start(); err != nil {
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start TUN: %w", err)
	}

	dns, err := newDNSServer(netip.AddrPortFrom(gateway, 53), pool, log)
	if err != nil {
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: dns server: %w", err)
	}
	dns.Start(ctx)

	// AutoRedirect's RouteExcludeAddressSet keeps the fake-IP CIDR
	// out of the OUTPUT REDIRECT rule, so TCP destined to fake-IPs
	// flows through the TUN (where our handler reverse-looks up the
	// hostname) instead of being DNAT'd to the userspace TCP listener
	// (where it would only see the IP, not the hostname).
	excludeIPSetBuilder := &netipx.IPSetBuilder{}
	excludeIPSetBuilder.AddPrefix(cfg.FakeIPPrefix)
	excludeIPSet, err := excludeIPSetBuilder.IPSet()
	if err != nil {
		_ = dns.Close()
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: build route-exclude set: %w", err)
	}
	routeExcludeSets := []*netipx.IPSet{excludeIPSet}
	emptyAddressSets := []*netipx.IPSet{}

	redirect, err := singtun.NewAutoRedirect(singtun.AutoRedirectOptions{
		TunOptions:             tunOpts,
		Context:                ctx,
		Handler:                h,
		Logger:                 singLog,
		NetworkMonitor:         netMon,
		InterfaceFinder:        finder,
		TableName:              "snell_tun",
		RouteAddressSet:        &emptyAddressSets,
		RouteExcludeAddressSet: &routeExcludeSets,
	})
	if err != nil {
		_ = dns.Close()
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: build auto-redirect: %w", err)
	}
	if err := redirect.Start(); err != nil {
		_ = dns.Close()
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start auto-redirect: %w", err)
	}

	log.Info("snell tun ready",
		"iface", cfg.TUNName,
		"gateway", gateway.String(),
		"fake-ip-prefix", cfg.FakeIPPrefix.String(),
		"mtu", cfg.MTU,
		"output-mark", fmt.Sprintf("0x%x", cfg.OutputMark),
		"exclude-uids", cfg.ExcludeUIDs,
	)

	return &inbound{
		redirect: redirect,
		dns:      dns,
		stack:    stack,
		device:   device,
		ifMon:    ifMon,
		netMon:   netMon,
		log:      log,
	}, nil
}

type inbound struct {
	closeOnce sync.Once
	redirect  singtun.AutoRedirect
	dns       *dnsServer
	stack     singtun.Stack
	device    singtun.Tun
	ifMon     singtun.DefaultInterfaceMonitor
	netMon    singtun.NetworkUpdateMonitor
	log       *slog.Logger
}

func (i *inbound) Close() error {
	var firstErr error
	rec := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	i.closeOnce.Do(func() {
		// AutoRedirect first: stops new redirected TCP from arriving.
		rec(i.redirect.Close())
		// DNS server next: stops new fake-IP allocations.
		rec(i.dns.Close())
		// Then the TUN device and its stack: tears down the data
		// path entirely.
		rec(i.device.Close())
		rec(i.stack.Close())
		// Monitors last.
		rec(i.ifMon.Close())
		rec(i.netMon.Close())
		i.log.Info("snell tun closed")
	})
	return firstErr
}

// handler bridges both inbound paths (TUN stack for fake-IP, AutoRedirect
// for everything else) to snell.Client. When destination falls inside
// the fake-IP pool's prefix, we reverse-lookup the original hostname so
// the snell server gets AtypDomainName and does its own (clean) DNS.
type handler struct {
	ctx    context.Context
	dialer Dialer
	log    *slog.Logger
	pool   *FakePool
}

// PrepareConnection — see comments in v1 implementation. Always defer
// the actual TCP handling to NewConnectionEx.
func (h *handler) PrepareConnection(
	_ string,
	_ M.Socksaddr,
	_ M.Socksaddr,
	_ singtun.DirectRouteContext,
	_ time.Duration,
) (singtun.DirectRouteDestination, error) {
	return nil, nil
}

func (h *handler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()

	host, port, ok := h.resolveDest(destination)
	if !ok {
		// fake-IP that has no pool entry — likely a scanner or a stale
		// cache. Drop silently to avoid leaking a connection attempt
		// for a destination the user never asked for.
		h.log.Debug("tun tcp: unknown fake-ip", "dst", destination.String(), "src", source.String())
		return
	}

	upstream, err := h.dialer.DialTCP(ctx, host, port)
	if err != nil {
		h.log.Debug("tun tcp: dial failed", "dst-host", host, "dst-port", port, "err", err)
		return
	}
	defer upstream.Close()

	h.log.Debug("tun tcp", "src", source.String(), "dst-orig", destination.String(), "dst-real", host)
	utils.Relay(conn, upstream)
}

// resolveDest returns the snell-side (host, port) for a given
// connection-arrival destination:
//
//  • Fake-IP destination (came from the TUN stack): pool lookup for
//    the original hostname. ok=false if the IP is in the fake-IP
//    prefix but unmapped.
//  • Real IP destination (came from AutoRedirect): pass through.
func (h *handler) resolveDest(dst M.Socksaddr) (host string, port uint16, ok bool) {
	addr := dst.Addr.Unmap()
	if h.pool.Contains(addr) {
		name, found := h.pool.Lookup(addr)
		if !found {
			return "", 0, false
		}
		return name, dst.Port, true
	}
	return dst.AddrString(), dst.Port, dst.Port != 0
}

// NewPacketConnectionEx is required by the Handler interface. We do
// not handle UDP in v2 (DNS is the only UDP we care about, and it's
// served directly by the in-process DNS server bound to the TUN
// gateway — sing-tun's DNS hijack DNATs UDP 53 there, so it never
// arrives via this packet-conn callback). Drop silently.
func (h *handler) NewPacketConnectionEx(_ context.Context, conn N.PacketConn, _ M.Socksaddr, _ M.Socksaddr, onClose N.CloseHandlerFunc) {
	_ = conn.Close()
	if onClose != nil {
		onClose(nil)
	}
}

// slogAdapter satisfies sing's logger.Logger by forwarding to slog.
type slogAdapter struct{ l *slog.Logger }

func newSlogAdapter(l *slog.Logger) logger.Logger { return &slogAdapter{l: l} }

func (s *slogAdapter) Trace(args ...any) { s.l.Debug("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Debug(args ...any) { s.l.Debug("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Info(args ...any)  { s.l.Info("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Warn(args ...any)  { s.l.Warn("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Error(args ...any) { s.l.Error("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Fatal(args ...any) { s.l.Error("sing-tun (fatal)", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Panic(args ...any) { s.l.Error("sing-tun (panic)", "msg", fmt.Sprint(args...)) }
