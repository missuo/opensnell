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

	pool4, err := NewFakePool(cfg.FakeIPPrefix, 0)
	if err != nil {
		return nil, fmt.Errorf("snell-tun: build fake-ip pool: %w", err)
	}
	pools := &FakePools{V4: pool4} // V6 intentionally nil — see Config.FakeIPPrefix doc.
	gateway := pool4.Gateway()
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

	// TUN address: assign the gateway with the prefix bits so the
	// kernel auto-routes the whole fake-IP CIDR over the TUN.
	tunPrefix := netip.PrefixFrom(gateway, cfg.FakeIPPrefix.Bits())

	tunOpts := &singtun.Options{
		Name:                   cfg.TUNName,
		Inet4Address:           []netip.Prefix{tunPrefix},
		MTU:                    cfg.MTU,
		AutoRedirectMarkMode:   true,
		AutoRedirectOutputMark: cfg.OutputMark,
		ExcludeUID:             excludeUIDs,
		// Hijack UDP/TCP :53 from apps and DNAT to our DNS server
		// listening on the TUN gateway. (TCP hijack lands on a port
		// we don't listen on — apps fall back to UDP, the common
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

	var prober *ipv6Prober
	if !cfg.DisableIPv6 {
		prober, err = newIPv6Prober(dialer, cfg.IPv6ProbeTarget, cfg.IPv6ProbeInterval, log)
		if err != nil {
			_ = ifMon.Close()
			_ = netMon.Close()
			return nil, fmt.Errorf("snell-tun: ipv6 prober: %w", err)
		}
	}

	h := &handler{ctx: ctx, dialer: dialer, log: log, pools: pools, ipv6: prober}

	h.tun = device
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

	if prober != nil {
		prober.Start(ctx)
	}

	// Bypass + Direct Domain wiring. We construct bypass before the
	// DNS server because the DNS server holds a reference to the
	// direct-DNS forwarder which mutates the bypass at runtime.
	// The AutoRedirect attaches to bypass below (after NewAutoRedirect
	// returns) so dynamic AddIP calls can re-publish nftables rules.
	bypass := newLinuxBypass(log)
	if err := bypass.AddCIDR(cfg.FakeIPPrefix); err != nil {
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: bypass add fake-ip: %w", err)
	}
	for _, p := range cfg.DirectIPs {
		if err := bypass.AddCIDR(p); err != nil {
			_ = device.Close()
			_ = stack.Close()
			_ = ifMon.Close()
			_ = netMon.Close()
			return nil, fmt.Errorf("snell-tun: bypass add direct-ip %s: %w", p, err)
		}
	}
	direct, err := newDirectDNS(cfg.UpstreamDNS, cfg.DirectDomains, bypass, cfg.OutputMark, log)
	if err != nil {
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: direct dns: %w", err)
	}

	dns, err := newDNSServer(netip.AddrPortFrom(gateway, 53), pools, direct, log)
	if err != nil {
		_ = device.Close()
		_ = stack.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: dns server: %w", err)
	}
	dns.Start(ctx)

	// AutoRedirect's RouteExcludeAddressSet keeps the fake-IP CIDR
	// (and any user-configured DirectIPs, plus dynamic IPs from the
	// Direct Domain DNS path) out of the OUTPUT REDIRECT rule, so
	// matching TCP flows are NOT DNAT'd to the userspace listener.
	// Bypass + static prefixes were initialized above.
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
		RouteExcludeAddressSet: bypass.routeSetPointer(),
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
	bypass.attach(ctx, redirect)

	log.Info("snell tun ready (linux / auto-redirect)",
		"iface", cfg.TUNName,
		"gateway", gateway.String(),
		"prefix", cfg.FakeIPPrefix.String(),
		"mtu", cfg.MTU,
		"output-mark", fmt.Sprintf("0x%x", cfg.OutputMark),
		"exclude-uids", cfg.ExcludeUIDs,
		"direct-ips", cfg.DirectIPs,
		"direct-domains", cfg.DirectDomains,
		"upstream-dns", cfg.UpstreamDNS,
	)

	return &inbound{
		redirect: redirect,
		dns:      dns,
		stack:    stack,
		device:   device,
		ifMon:    ifMon,
		netMon:   netMon,
		prober:   prober,
		bypass:   bypass,
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
	prober    *ipv6Prober
	bypass    *linuxBypass
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
		// Prober is independent of the data path; stop it after the
		// data path is fully torn down so an in-flight probe dial
		// does not race against the dialer close.
		i.prober.Close()
		// Bypass reaper stops here — UpdateRouteAddressSet calls go
		// through AutoRedirect which is already closed above; reaper
		// republishes would be no-ops anyway.
		_ = i.bypass.Close()
		i.log.Info("snell tun closed")
	})
	return firstErr
}

// handler bridges both inbound paths (TUN stack for fake-IP, AutoRedirect
// for everything else) to snell.Client. When destination falls inside
// the fake-IP pool's prefix, we reverse-lookup the original hostname so
// the snell server gets AtypDomainName and does its own (clean) DNS.
// Holds both v4 and v6 fake-IP pools so the same handler instance
// serves both inbound paths (TUN system stack + AutoRedirect TCP
// listener).
type handler struct {
	ctx    context.Context
	dialer Dialer
	log    *slog.Logger
	pools  *FakePools
	// ipv6 is nil iff DisableIPv6 was set in config. When non-nil,
	// handler queries Allowed() to gate real (non-fake-IP) v6
	// destinations: refusing the connection lets happy-eyeballs apps
	// fall back to A quickly instead of hanging on a v6 dial the
	// snell server will never complete.
	ipv6 *ipv6Prober
	// tun is the device used to inject synthetic packets back to the
	// app (currently: ICMP Port Unreachable for UDP/443 to trigger
	// QUIC → TCP fallback). nil during early init; set before the
	// stack is started.
	tun singtun.Tun
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

	if h.shouldDropV6(destination) {
		h.log.Debug("tun tcp: drop real ipv6 (server v6 path not available)",
			"dst", destination.String(), "src", source.String())
		return
	}

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

// shouldDropV6 reports whether the destination is a real (non-fake-IP)
// IPv6 address that we must reject because the server is known to
// have no working v6 path. Fake-IPv6 destinations (when configured)
// are always allowed — those tunnel via AtypDomainName regardless.
func (h *handler) shouldDropV6(dst M.Socksaddr) bool {
	addr := dst.Addr.Unmap()
	if !addr.IsValid() || addr.Is4() {
		return false
	}
	if h.pools.Contains(addr) {
		return false
	}
	// Real v6 destination. Drop iff v6 is fully disabled OR the
	// runtime probe says the server cannot reach v6.
	if h.ipv6 == nil {
		return true
	}
	return !h.ipv6.Allowed()
}

// resolveDest returns the snell-side (host, port) for a given
// connection-arrival destination:
//
//  • Fake-IP destination (came from the TUN stack, either v4 or v6):
//    pool lookup for the original hostname. ok=false if the IP is in
//    a fake-IP prefix but unmapped.
//  • Real IP destination (came from AutoRedirect): pass through.
func (h *handler) resolveDest(dst M.Socksaddr) (host string, port uint16, ok bool) {
	addr := dst.Addr.Unmap()
	if h.pools.Contains(addr) {
		name, found := h.pools.Lookup(addr)
		if !found {
			return "", 0, false
		}
		return name, dst.Port, true
	}
	return dst.AddrString(), dst.Port, dst.Port != 0
}

// NewPacketConnectionEx is required by the Handler interface. The
// v2 TUN does not proxy UDP application traffic, but we treat
// UDP/443 (web QUIC) specially: instead of silently dropping it
// (forcing the browser to wait ~10s before falling back to TCP),
// we inject an ICMP Port Unreachable back to the app so QUIC
// abandons immediately and switches to HTTP/2 over TCP. Every
// other UDP flow is still dropped silently — sending ICMP for
// random UDP services (games, VoIP) could break those apps.
//
// DNS UDP never reaches here on Linux: sing-tun's nft DNS hijack
// DNATs UDP/53 to our fake-IP DNS server at the gateway address,
// which has its own listener.
func (h *handler) NewPacketConnectionEx(_ context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()
	if destination.Port == quicTriggerPort && h.tun != nil {
		src := netip.AddrPortFrom(source.Addr.Unmap(), source.Port)
		dst := netip.AddrPortFrom(destination.Addr.Unmap(), destination.Port)
		if err := sendQUICPortUnreachable(h.tun, src, dst); err != nil {
			h.log.Debug("tun udp: icmp unreachable inject failed",
				"src", src.String(), "dst", dst.String(), "err", err)
		} else {
			h.log.Debug("tun udp: quic fallback icmp sent",
				"src", src.String(), "dst", dst.String())
		}
		return
	}
	h.log.Debug("tun udp: drop non-quic", "src", source.String(), "dst", destination.String())
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
