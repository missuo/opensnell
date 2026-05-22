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
	"io"
	"log/slog"
	"net"
	"net/netip"
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

// New starts the TUN inbound. The returned Inbound runs until its Close
// method is called or ctx is cancelled (Close is still required to clean
// up sing-tun's kernel routes/rules — context cancellation alone does
// not unset them).
func New(ctx context.Context, cfg Config, dialer Dialer, log *slog.Logger) (Inbound, error) {
	if log == nil {
		log = slog.Default()
	}
	if dialer == nil {
		return nil, errors.New("snell-tun: dialer is required")
	}
	if !cfg.Address.IsValid() || !cfg.Address.Addr().Is4() {
		return nil, fmt.Errorf("snell-tun: invalid TUN address %q (need IPv4 CIDR)", cfg.Address)
	}
	if cfg.MTU == 0 {
		cfg.MTU = 9000
	}
	if cfg.Name == "" {
		cfg.Name = singtun.CalculateInterfaceName("tun")
	}

	excludes := make([]netip.Prefix, 0, len(cfg.ServerIPs))
	for _, ip := range cfg.ServerIPs {
		if !ip.IsValid() {
			continue
		}
		// Sing-tun matches Inet4RouteExcludeAddress per address family.
		// IPv6 server IPs would also need Inet6RouteExcludeAddress, but
		// the v1 TUN config is IPv4-only on the inside, so we just skip
		// non-v4 here. The kernel still routes to the v6 server via its
		// own default route (TUN never claims default v6).
		if ip.Is4() {
			excludes = append(excludes, netip.PrefixFrom(ip, 32))
		}
	}

	singLog := newSlogAdapter(log)

	// Linux requires both monitors even when we don't care about Android
	// VPN / interface flapping — tun.NativeTun.Start dereferences
	// InterfaceMonitor unconditionally (unless EXP_ExternalConfiguration).
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

	opts := singtun.Options{
		Name:                     cfg.Name,
		Inet4Address:             []netip.Prefix{cfg.Address},
		MTU:                      cfg.MTU,
		AutoRoute:                cfg.AutoRoute,
		Inet4RouteExcludeAddress: excludes,
		InterfaceFinder:          finder,
		InterfaceMonitor:         ifMon,
		Logger:                   singLog,
	}

	device, err := singtun.New(opts)
	if err != nil {
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: create TUN: %w", err)
	}

	h := &handler{ctx: ctx, dialer: dialer, log: log}

	// "system" stack works without the with_gvisor build tag and keeps
	// the binary lean. It handles TCP/UDP via raw IP packet rewriting in
	// userspace — no kernel netfilter rules required.
	stack, err := singtun.NewStack("system", singtun.StackOptions{
		Context:         ctx,
		Tun:             device,
		TunOptions:      opts,
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
		_ = device.Close()
		_ = ifMon.Close()
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start TUN: %w", err)
	}

	log.Info("snell tun up",
		"iface", cfg.Name,
		"addr", cfg.Address.String(),
		"mtu", cfg.MTU,
		"auto-route", cfg.AutoRoute,
		"server-excluded", len(excludes),
	)

	return &inbound{
		stack:  stack,
		device: device,
		ifMon:  ifMon,
		netMon: netMon,
		log:    log,
	}, nil
}

type inbound struct {
	closeOnce sync.Once
	stack     singtun.Stack
	device    singtun.Tun
	ifMon     singtun.DefaultInterfaceMonitor
	netMon    singtun.NetworkUpdateMonitor
	log       *slog.Logger
}

func (i *inbound) Close() error {
	var firstErr error
	i.closeOnce.Do(func() {
		// device.Close before stack.Close — device.Close unsets routes
		// and ip rules. If we tear the stack down first, in-flight
		// userspace forwarders might error out noisily before the
		// kernel-side rules are gone.
		if err := i.device.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := i.stack.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := i.ifMon.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := i.netMon.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	return firstErr
}

// handler bridges sing-tun's TCP/UDP callbacks to snell.Client. The TUN
// stack delivers IP-only destinations (no FQDN), so DialTCP gets an IPv4
// or IPv6 literal as host — snell encodes it as AtypIPv4/v6 on the wire.
type handler struct {
	ctx    context.Context
	dialer Dialer
	log    *slog.Logger
}

// PrepareConnection is sing-tun's pre-flight hook. Returning (nil, nil)
// means "let NewConnectionEx / NewPacketConnectionEx handle it" — we
// don't use any direct-route fast path.
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

	host := destination.AddrString()
	port := destination.Port
	if host == "" || port == 0 {
		h.log.Debug("tun tcp: empty destination", "src", source.String())
		return
	}

	upstream, err := h.dialer.DialTCP(ctx, host, port)
	if err != nil {
		h.log.Debug("tun tcp: dial failed", "dst", destination.String(), "err", err)
		return
	}
	defer upstream.Close()

	h.log.Debug("tun tcp", "src", source.String(), "dst", destination.String())
	utils.Relay(conn, upstream)
}

func (h *handler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()

	upstream, err := h.dialer.DialUDP(ctx)
	if err != nil {
		h.log.Debug("tun udp: dial failed", "err", err)
		return
	}
	defer upstream.Close()

	h.log.Debug("tun udp", "src", source.String(), "dst", destination.String())
	relayUDP(ctx, conn, upstream, destination)
}

// relayUDP pumps datagrams in both directions between the TUN-side
// PacketConn (sing's buffer-based API) and the snell UDP-associate
// upstream (plain net.PacketConn).
//
// Direction-from-TUN: each ReadPacket gives us a sing buf.Buffer and the
// per-packet destination as a Socksaddr. We forward to the upstream
// snell UDP-relay via WriteTo with the resolved UDPAddr.
//
// Direction-from-upstream: the snell relay returns (n, remoteAddr) where
// remoteAddr is the *external* peer (the actual destination). We surface
// it back to the TUN stack as the source of the reply — sing-tun's UDP
// NAT uses (source, destination) to route packets to the right local
// flow.
func relayUDP(ctx context.Context, tunPC N.PacketConn, snellPC net.PacketConn, _ M.Socksaddr) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// TUN -> snell
	go func() {
		defer cancel()
		for {
			buffer := buf.NewPacket()
			dst, err := tunPC.ReadPacket(buffer)
			if err != nil {
				buffer.Release()
				return
			}
			udpDst := dst.UDPAddr()
			if udpDst == nil {
				buffer.Release()
				continue
			}
			if _, err := snellPC.WriteTo(buffer.Bytes(), udpDst); err != nil {
				buffer.Release()
				return
			}
			buffer.Release()
		}
	}()

	// snell -> TUN
	go func() {
		defer cancel()
		buffer := make([]byte, 64*1024)
		for {
			_ = snellPC.SetReadDeadline(time.Now().Add(5 * time.Minute))
			n, raddr, err := snellPC.ReadFrom(buffer)
			if err != nil {
				if errors.Is(err, io.EOF) || isTimeout(err) {
					return
				}
				return
			}
			src := M.SocksaddrFromNet(raddr)
			b := buf.As(buffer[:n])
			if err := tunPC.WritePacket(b, src); err != nil {
				return
			}
		}
	}()

	<-ctx.Done()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// slogAdapter satisfies sing's logger.Logger by forwarding to slog. Fatal
// and Panic are emitted at Error level — we never want a third-party
// logger to actually os.Exit or panic.
type slogAdapter struct{ l *slog.Logger }

func newSlogAdapter(l *slog.Logger) logger.Logger { return &slogAdapter{l: l} }

func (s *slogAdapter) Trace(args ...any) { s.l.Debug("sing-tun", "msg", joinArgs(args)) }
func (s *slogAdapter) Debug(args ...any) { s.l.Debug("sing-tun", "msg", joinArgs(args)) }
func (s *slogAdapter) Info(args ...any)  { s.l.Info("sing-tun", "msg", joinArgs(args)) }
func (s *slogAdapter) Warn(args ...any)  { s.l.Warn("sing-tun", "msg", joinArgs(args)) }
func (s *slogAdapter) Error(args ...any) { s.l.Error("sing-tun", "msg", joinArgs(args)) }
func (s *slogAdapter) Fatal(args ...any) { s.l.Error("sing-tun (fatal)", "msg", joinArgs(args)) }
func (s *slogAdapter) Panic(args ...any) { s.l.Error("sing-tun (panic)", "msg", joinArgs(args)) }

func joinArgs(args []any) string {
	return fmt.Sprint(args...)
}
