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

// New brings up the auto-redirect TCP inbound. The nftables rules
// installed by sing-tun stay active until Close is called; SIGTERM via
// the parent context cancels the handler but does not by itself unwind
// the kernel rules, so the caller must invoke Close on shutdown.
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

	singLog := newSlogAdapter(log)

	// AutoRedirect tracks the host's interface addresses (to know which
	// destinations to leave alone) via the InterfaceFinder + a network
	// update monitor. Without these, the package can still install the
	// nftables rules but will not pick up later interface address
	// changes.
	finder := control.NewDefaultInterfaceFinder()
	netMon, err := singtun.NewNetworkUpdateMonitor(singLog)
	if err != nil {
		return nil, fmt.Errorf("snell-tun: network monitor: %w", err)
	}
	if err := netMon.Start(); err != nil {
		return nil, fmt.Errorf("snell-tun: start network monitor: %w", err)
	}

	excludeUIDs := make([]ranges.Range[uint32], 0, len(cfg.ExcludeUIDs))
	for _, uid := range cfg.ExcludeUIDs {
		excludeUIDs = append(excludeUIDs, ranges.NewSingle(uid))
	}

	// TunOptions for the auto-redirect path is mostly bookkeeping: we
	// only really care about AutoRedirectOutputMark + ExcludeUID. The
	// non-zero Inet4Address is what makes sing-tun enable the IPv4
	// portion of the nftables rule set; the address itself never gets
	// assigned to any interface because we deliberately do not create a
	// TUN device for the TCP-only redirect path.
	//
	// AutoRedirectMarkMode = true is critical: without it, sing-tun's
	// nftables OUTPUT chain has no mark-based bypass rule (see
	// redirect_nftables_rules.go ~line 135), and our own snell-client
	// → snell-server TCP would be captured by the very redirect rule
	// we just installed, looping infinitely. With mark-mode on, our
	// SO_MARK'd packets match the bypass and go out normally.
	tunOpts := &singtun.Options{
		Name:                   "snell-tun-noop",
		Inet4Address:           []netip.Prefix{netip.MustParsePrefix("198.18.0.1/16")},
		AutoRedirectMarkMode:   true,
		AutoRedirectOutputMark: cfg.OutputMark,
		ExcludeUID:             excludeUIDs,
		InterfaceFinder:        finder,
		Logger:                 singLog,
		// Don't hijack DNS — DNS resolution should stay on the box's
		// configured resolver (which already works); we only need TCP
		// captured.
		EXP_DisableDNSHijack:      true,
		EXP_ExternalConfiguration: true,
	}

	// AutoRedirect's nftables rules accept these as pointers so callers
	// can mutate the route-address sets at runtime. We don't use the
	// feature but the autoRedirect implementation dereferences both
	// pointers, so they cannot be nil.
	var emptySet []*netipx.IPSet
	h := &handler{ctx: ctx, dialer: dialer, log: log}
	redirect, err := singtun.NewAutoRedirect(singtun.AutoRedirectOptions{
		TunOptions:             tunOpts,
		Context:                ctx,
		Handler:                h,
		Logger:                 singLog,
		NetworkMonitor:         netMon,
		InterfaceFinder:        finder,
		TableName:              "snell_tun",
		RouteAddressSet:        &emptySet,
		RouteExcludeAddressSet: &emptySet,
	})
	if err != nil {
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: build auto-redirect: %w", err)
	}
	if err := redirect.Start(); err != nil {
		_ = netMon.Close()
		return nil, fmt.Errorf("snell-tun: start auto-redirect: %w", err)
	}

	log.Info("snell tun (auto-redirect) up",
		"output-mark", fmt.Sprintf("0x%x", cfg.OutputMark),
		"exclude-uids", cfg.ExcludeUIDs,
	)

	return &inbound{
		redirect: redirect,
		netMon:   netMon,
		log:      log,
	}, nil
}

type inbound struct {
	closeOnce sync.Once
	redirect  singtun.AutoRedirect
	netMon    singtun.NetworkUpdateMonitor
	log       *slog.Logger
}

func (i *inbound) Close() error {
	var firstErr error
	i.closeOnce.Do(func() {
		// AutoRedirect.Close pulls the nftables table down and stops
		// the userspace TCP listener. Do that first so no new traffic
		// is in-flight before we drop the rest of the bookkeeping.
		if err := i.redirect.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := i.netMon.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		i.log.Info("snell tun closed")
	})
	return firstErr
}

// handler bridges sing-tun's TCP redirect callback to snell.Client.
// In auto-redirect mode the redirected conn is a real TCP socket
// accepted by sing-tun's redirectServer (with SO_ORIGINAL_DST already
// resolved into `destination`), so we get a clean (src, dst) pair and
// can just relay through snell.DialTCP.
type handler struct {
	ctx    context.Context
	dialer Dialer
	log    *slog.Logger
}

// PrepareConnection is sing-tun's pre-flight hook. We don't use any
// direct-route fast path, so always defer to NewConnectionEx.
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

// NewPacketConnectionEx is required by the Handler interface but never
// invoked in auto-redirect mode (UDP is not redirected by the v1
// implementation). Provide a no-op so we still satisfy the interface.
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
