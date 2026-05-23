/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build darwin

package tun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	singtun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/missuo/opensnell/components/utils"
)

// New brings up the macOS TUN inbound. Architecture differs from
// Linux because macOS has no nftables / netfilter — sing-tun's
// AutoRedirect path is unavailable. So we go all-in on the TUN's
// default-route capture:
//
//   1. Create a utun device with the fake-IP CIDR as its address.
//   2. AutoRoute=true → sing-tun installs sub-prefix routes covering
//      0.0.0.0/0 (1/8 + 2/7 + 4/6 + … + 128/1) via the TUN, with
//      ServerIPs carved out as Inet4RouteExcludeAddress so our own
//      outbound to the snell server stays on the real interface.
//   3. The TUN's userspace "system" stack catches every TCP and UDP
//      flow that the kernel routes through it. The handler then:
//        - For fake-IP destinations → reverse-look-up the hostname
//          and dial through snell with AtypDomainName.
//        - For real-IP destinations → pass through, dial through snell
//          with AtypIPv4.
//        - For UDP destined to port 53 → answer in-process with the
//          fake-IP DNS responder. No upstream forwarding.
//        - For other UDP → drop (v2 does not proxy UDP).
//
// Cleanup is sing-tun-native: device.Close() reverses every route the
// setRoutes() call added.
func New(ctx context.Context, cfg Config, dialer Dialer, log *slog.Logger) (Inbound, error) {
	if log == nil {
		log = slog.Default()
	}
	if dialer == nil {
		return nil, errors.New("snell-tun: dialer is required")
	}
	if cfg.MTU == 0 {
		// macOS utun caps at the underlying interface's MTU minus a
		// few bytes; staying at 1500 avoids userspace stack
		// fragmentation surprises.
		cfg.MTU = 1500
	}
	if cfg.TUNName == "" {
		cfg.TUNName = singtun.CalculateInterfaceName("utun")
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
	pools := &FakePools{V4: pool4} // V6 intentionally nil — IPv4-only by design.
	gateway := pool4.Gateway()
	log.Info("snell tun fake-ip pool ready",
		"prefix", cfg.FakeIPPrefix.String(),
		"gateway", gateway.String(),
	)

	excludeIPv4 := make([]netip.Prefix, 0, len(cfg.ServerIPs))
	for _, ip := range cfg.ServerIPs {
		if !ip.IsValid() || !ip.Is4() {
			// IPv6 server IPs are ignored: TUN is v4-only, so the
			// kernel doesn't try to route v6 destinations through us
			// in the first place. Caller can configure server as a
			// v4 IP and we handle exclusion there.
			continue
		}
		excludeIPv4 = append(excludeIPv4, netip.PrefixFrom(ip, 32))
	}

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

	// TUN address = pool gateway with the pool's prefix bits. Kernel
	// auto-routes the fake-IP CIDR via the TUN. AutoRoute additionally
	// installs IPv4 sub-prefixes covering the rest of v4 space (minus
	// the snell server exclusions). v6 is left untouched on purpose —
	// see the FakeIPPrefix doc on Config.
	tunPrefix := netip.PrefixFrom(gateway, cfg.FakeIPPrefix.Bits())

	tunOpts := singtun.Options{
		Name:                     cfg.TUNName,
		Inet4Address:             []netip.Prefix{tunPrefix},
		MTU:                      cfg.MTU,
		AutoRoute:                true,
		Inet4RouteExcludeAddress: excludeIPv4,
		InterfaceFinder:          finder,
		InterfaceMonitor:         ifMon,
		Logger:                   singLog,
	}

	device, err := singtun.New(tunOpts)
	if err != nil {
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: create utun device: %w", err)
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

	h := &darwinHandler{
		ctx:    ctx,
		dialer: dialer,
		log:    log,
		pools:  pools,
		ipv6:   prober,
		tun:    device,
	}

	stack, err := singtun.NewStack("system", singtun.StackOptions{
		Context:         ctx,
		Tun:             device,
		TunOptions:      tunOpts,
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
		return nil, fmt.Errorf("snell-tun: start utun: %w", err)
	}

	if prober != nil {
		prober.Start(ctx)
	}

	// Bypass manager + Direct Domain wiring. The bypass impl shells
	// out to `route` and persists added entries so a SIGKILL'd
	// previous run can have its routes reclaimed at startup. The
	// reclaim runs synchronously inside newDarwinBypass.
	bypass, err := newDarwinBypass(ctx, "", log)
	if err != nil {
		_ = stack.Close()
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: bypass init: %w", err)
	}
	for _, p := range cfg.DirectIPs {
		if err := bypass.AddCIDR(p); err != nil {
			_ = bypass.Close()
			_ = stack.Close()
			_ = device.Close()
			_ = ifMon.Close()
			_ = netMon.Close()
			return nil, fmt.Errorf("snell-tun: bypass add direct-ip %s: %w", p, err)
		}
	}
	direct, err := newDirectDNS(cfg.UpstreamDNS, cfg.DirectDomains, bypass, 0, log)
	if err != nil {
		_ = bypass.Close()
		_ = stack.Close()
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: direct dns: %w", err)
	}
	h.direct = direct

	// Override the host's system DNS to point at our gateway. macOS's
	// mDNSResponder otherwise uses per-network-service DNS scoping —
	// it binds queries to the Wi-Fi / Ethernet source interface and
	// sends them out THAT interface, completely bypassing the TUN
	// default route. By replacing the system DNS with the TUN
	// gateway IP (which only lives on the utun device), every
	// scoped query is forced through us. Stored backups are
	// restored in Close().
	dnsBackups, err := overrideSystemDNS(gateway, log)
	if err != nil {
		_ = stack.Close()
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: override system dns: %w", err)
	}

	log.Info("snell tun ready (darwin / auto-route + system dns override)",
		"iface", cfg.TUNName,
		"gateway", gateway.String(),
		"prefix", cfg.FakeIPPrefix.String(),
		"mtu", cfg.MTU,
		"exclude-ips", cfg.ServerIPs,
		"dns-overrides", len(dnsBackups),
		"direct-ips", cfg.DirectIPs,
		"direct-domains", cfg.DirectDomains,
		"upstream-dns", cfg.UpstreamDNS,
	)

	return &darwinInbound{
		stack:      stack,
		device:     device,
		ifMon:      ifMon,
		netMon:     netMon,
		dnsBackups: dnsBackups,
		prober:     prober,
		bypass:     bypass,
		log:        log,
	}, nil
}

type darwinInbound struct {
	closeOnce  sync.Once
	stack      singtun.Stack
	device     singtun.Tun
	ifMon      singtun.DefaultInterfaceMonitor
	netMon     singtun.NetworkUpdateMonitor
	dnsBackups []dnsBackup
	prober     *ipv6Prober
	bypass     *darwinBypass
	log        *slog.Logger
}

func (i *darwinInbound) Close() error {
	var firstErr error
	rec := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	i.closeOnce.Do(func() {
		// Restore system DNS FIRST — even if everything else tearing
		// down fails, we don't want to strand the host with its
		// resolver pointed at an about-to-be-destroyed utun.
		restoreSystemDNS(i.dnsBackups, i.log)

		// device.Close before stack.Close — device.Close runs
		// unsetRoutes which removes the auto-route entries. Tearing
		// the stack down first would let userspace forwarders error
		// out noisily while the kernel still has TUN routes pointed
		// at a fading interface.
		rec(i.device.Close())
		rec(i.stack.Close())
		rec(i.ifMon.Close())
		rec(i.netMon.Close())
		i.prober.Close()
		// Bypass close runs after the data path is fully torn down,
		// so route deletes don't race against in-flight DNS-forwarder
		// dynamic AddIPs. Also writes a final empty state file so
		// the next clean start has nothing to reclaim.
		rec(i.bypass.Close())
		i.log.Info("snell tun closed")
	})
	return firstErr
}

// darwinHandler bridges sing-tun's TCP + UDP callbacks to snell.Client
// and the in-process fake-IP DNS responder. Holds both v4 and v6
// pools — TCP arrives with either family, and the DNS responder
// handles A vs AAAA via the appropriate pool.
type darwinHandler struct {
	ctx    context.Context
	dialer Dialer
	log    *slog.Logger
	pools  *FakePools
	// ipv6 gates real (non-fake-IP) v6 destinations on the runtime
	// probe of server v6 reachability. Nil iff config disabled v6.
	// See handler.shouldDropV6 in tun_linux.go for the rationale.
	ipv6 *ipv6Prober
	// tun is the device used to inject ICMP Port Unreachable so
	// QUIC apps fall back to TCP fast. Mirrors handler.tun in
	// tun_linux.go.
	tun singtun.Tun
	// direct routes Direct-Domain DNS queries to the configured
	// upstream resolver and registers returned IPs with the bypass
	// manager. Nil = feature disabled (no DirectDomains in config).
	direct *directDNS
}

// PrepareConnection — defer everything to NewConnectionEx /
// NewPacketConnectionEx; no direct-route fast path.
func (h *darwinHandler) PrepareConnection(
	_ string,
	_ M.Socksaddr,
	_ M.Socksaddr,
	_ singtun.DirectRouteContext,
	_ time.Duration,
) (singtun.DirectRouteDestination, error) {
	return nil, nil
}

func (h *darwinHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()

	// TCP to port 53 — apps falling back from UDP DNS. v2 does not
	// implement the TCP DNS path; drop and let the app retry UDP
	// (where our in-process responder will pick it up).
	if destination.Port == 53 {
		h.log.Debug("tun tcp: drop tcp dns (v2 not supported)", "dst", destination.String())
		return
	}

	if h.shouldDropV6(destination) {
		h.log.Debug("tun tcp: drop real ipv6 (server v6 path not available)",
			"dst", destination.String(), "src", source.String())
		return
	}

	host, port, ok := h.resolveDest(destination)
	if !ok {
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

// shouldDropV6 mirrors handler.shouldDropV6 in tun_linux.go — see
// that comment. Lives in this file because darwinHandler is a
// separate type.
func (h *darwinHandler) shouldDropV6(dst M.Socksaddr) bool {
	addr := dst.Addr.Unmap()
	if !addr.IsValid() || addr.Is4() {
		return false
	}
	if h.pools.Contains(addr) {
		return false
	}
	if h.ipv6 == nil {
		return true
	}
	return !h.ipv6.Allowed()
}

// resolveDest returns the snell-side (host, port) tuple:
//
//   - Fake-IP destination (either v4 or v6): pool reverse-lookup.
//     ok=false if the IP is in a fake-IP prefix but unmapped (a stray
//     packet from an LRU-evicted entry, or a scanner) — caller drops.
//   - Real-IP destination: pass through as an IP literal so the snell
//     server connects to it directly. AtypIPv4 / AtypIPv6.
func (h *darwinHandler) resolveDest(dst M.Socksaddr) (host string, port uint16, ok bool) {
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

// NewPacketConnectionEx handles UDP flows that the TUN stack picked
// up. We only do anything for DNS (UDP port 53), which we answer
// in-process from the fake-IP pool. Every other UDP destination is
// dropped — v2 does not proxy UDP application traffic.
func (h *darwinHandler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()

	if destination.Port != 53 {
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
		h.log.Debug("tun udp: drop non-dns", "src", source.String(), "dst", destination.String())
		return
	}

	// DNS over UDP: a single query → single reply is the normal case.
	// Resolvers do occasionally reuse the source port for multiple
	// queries, so loop until the PacketConn closes (sing-tun's UDP
	// NAT expires the flow after UDPTimeout silence).
	for {
		buffer := buf.NewPacket()
		dnsDst, err := conn.ReadPacket(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		_ = dnsDst // not used: every packet on this flow targets `destination`

		resp, ok := ServeDNSQuery(buffer.Bytes(), h.pools, h.direct, h.log)
		buffer.Release()
		if !ok || len(resp) == 0 {
			continue
		}

		out := buf.As(resp)
		// destination here is the original (e.g. 8.8.8.8:53) — sing-tun
		// uses it as the apparent source of the reply, so the app sees
		// the response coming from the resolver it asked. WritePacket
		// releases the buffer internally on success.
		if err := conn.WritePacket(out, destination); err != nil {
			h.log.Debug("tun udp: dns write reply", "err", err)
			return
		}
	}
}

// ---- macOS system DNS override (scutil/networksetup wrapper) ----
//
// macOS's mDNSResponder uses per-network-service DNS configuration
// and scopes outbound DNS packets to the interface that owns the
// configured resolver (Wi-Fi → en0, Ethernet → en1, …). That means
// even after we install a TUN default route via auto-route, the OS
// happily sends DNS to e.g. `[240e:46c:1000:3fa7::9a]:53` out the
// Wi-Fi interface directly, bypassing the TUN — and our fake-IP DNS
// server never sees a query.
//
// The fix is to replace the system-configured DNS for every active
// network service with our TUN gateway IP. The gateway only exists
// on the utun interface, so scoped routing has nowhere to send the
// query except into our TUN, where our DNS server handles it. On
// shutdown we restore each service to whatever it had before
// (empty / DHCP / explicit servers).

// dnsBackup remembers one network service's pre-override DNS state.
// nil/empty `servers` means the service was on DHCP (no manually
// configured DNS) — restoration emits the literal token "Empty" to
// hand the service back to DHCP.
type dnsBackup struct {
	service string
	servers []string
}

// overrideSystemDNS sets every active network service's DNS to the
// TUN gateway and returns a list of per-service backups for later
// restoration. Errors on individual services are logged but not
// fatal — best-effort.
//
// If a service is already configured for our gateway (e.g. left over
// from a previous crashed run), we record an EMPTY backup so that
// restore puts it back on DHCP, rather than leaving it pointing at a
// gateway that no longer exists.
func overrideSystemDNS(gateway netip.Addr, log *slog.Logger) ([]dnsBackup, error) {
	services, err := listNetworkServices()
	if err != nil {
		return nil, fmt.Errorf("list network services: %w", err)
	}
	gatewayStr := gateway.String()
	var backups []dnsBackup
	for _, svc := range services {
		current, err := getDNSServers(svc)
		if err != nil {
			log.Warn("get dns failed; skipping service", "service", svc, "err", err)
			continue
		}
		backup := dnsBackup{service: svc, servers: current}
		// Detect "we already own this" — a previous run crashed
		// without restoring. The right thing to do is restore to
		// Empty (DHCP) on our own exit, since whatever the user
		// originally had is lost.
		for _, ip := range current {
			if ip == gatewayStr {
				backup.servers = nil
				break
			}
		}
		if err := setDNSServers(svc, []string{gatewayStr}); err != nil {
			log.Warn("set dns failed; skipping service", "service", svc, "err", err)
			continue
		}
		backups = append(backups, backup)
		log.Debug("dns override applied", "service", svc, "prev", current, "now", gatewayStr)
	}
	// Make absolutely sure mDNSResponder picks up the change before
	// the first app does its first lookup.
	_ = exec.Command("dscacheutil", "-flushcache").Run()
	_ = exec.Command("killall", "-HUP", "mDNSResponder").Run()
	return backups, nil
}

// restoreSystemDNS reverts every captured backup. Best-effort: each
// service is attempted independently and failures are logged. Empty
// `servers` restores the service to DHCP via the "Empty" token.
func restoreSystemDNS(backups []dnsBackup, log *slog.Logger) {
	for _, b := range backups {
		args := b.servers
		if len(args) == 0 {
			args = []string{"Empty"}
		}
		if err := setDNSServers(b.service, args); err != nil {
			log.Warn("restore dns failed", "service", b.service, "err", err)
		} else {
			log.Debug("dns restored", "service", b.service, "to", args)
		}
	}
	// Flush again so the restoration takes effect immediately.
	_ = exec.Command("dscacheutil", "-flushcache").Run()
	_ = exec.Command("killall", "-HUP", "mDNSResponder").Run()
}

// listNetworkServices returns the names of all enabled network
// services (Wi-Fi, Ethernet, etc.). Disabled services are excluded;
// `networksetup` marks them with a leading `*` and skipping them
// avoids spurious errors from `setdnsservers` on inactive entries.
func listNetworkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, err
	}
	var services []string
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// First line is a human-readable header ("An asterisk …").
		if i == 0 {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

// getDNSServers returns the DNS resolvers explicitly configured for
// `service` (i.e. ignoring DHCP-supplied entries). Empty result means
// "service is on DHCP / no manual config" — we represent that as a
// nil slice so the restore path can use the "Empty" token.
func getDNSServers(service string) ([]string, error) {
	out, err := exec.Command("networksetup", "-getdnsservers", service).Output()
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(out))
	// `networksetup` prints "There aren't any DNS Servers set on <svc>"
	// when nothing is configured. Detect that and return nil.
	if strings.HasPrefix(strings.ToLower(body), "there aren't") {
		return nil, nil
	}
	var servers []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		servers = append(servers, line)
	}
	return servers, nil
}

// setDNSServers applies dns servers to `service`. The token "Empty"
// is special — it tells macOS to drop manual config and fall back
// to DHCP.
func setDNSServers(service string, dns []string) error {
	args := append([]string{"-setdnsservers", service}, dns...)
	cmd := exec.Command("networksetup", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("networksetup %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// slogAdapter satisfies sing's logger.Logger by forwarding to slog.
// Duplicated with tun_linux.go intentionally — keeps each platform
// file self-contained and avoids a non-trivial cross-platform helper
// file just for this small glue type.
type slogAdapter struct{ l *slog.Logger }

func newSlogAdapter(l *slog.Logger) logger.Logger { return &slogAdapter{l: l} }

func (s *slogAdapter) Trace(args ...any) { s.l.Debug("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Debug(args ...any) { s.l.Debug("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Info(args ...any)  { s.l.Info("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Warn(args ...any)  { s.l.Warn("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Error(args ...any) { s.l.Error("sing-tun", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Fatal(args ...any) { s.l.Error("sing-tun (fatal)", "msg", fmt.Sprint(args...)) }
func (s *slogAdapter) Panic(args ...any) { s.l.Error("sing-tun (panic)", "msg", fmt.Sprint(args...)) }
