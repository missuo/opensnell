/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/ini.v1"

	"github.com/missuo/opensnell/components/snell"
	"github.com/missuo/opensnell/components/tun"
)

type clientFileConfig struct {
	listen   string
	snellCfg snell.ClientConfig
	tun      tunFileConfig
}

type tunFileConfig struct {
	enable    bool
	iface     string
	address   string
	mtu       uint32
	autoRoute bool
}

func main() {
	var (
		configPath string
		verbose    bool
		tunFlag    bool
	)
	flag.StringVar(&configPath, "c", "/etc/snell-server/snell-client.conf", "path to ini config file")
	flag.BoolVar(&verbose, "v", false, "enable verbose logging")
	flag.BoolVar(&tunFlag, "tun", false, "force-enable TUN inbound (overrides [snell-tun] enable=false; linux only)")
	flag.Parse()

	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	cfg, err := loadClientConfig(configPath)
	if err != nil {
		logger.Error("load config failed", "path", configPath, "err", err)
		os.Exit(1)
	}
	if tunFlag {
		cfg.tun.enable = true
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// In TUN mode we must lock in the snell server's IP *before* the TUN
	// becomes the default route — otherwise a subsequent re-resolve of
	// the server hostname would itself traverse the TUN and loop. We
	// also feed the resolved IPs to sing-tun's auto-route exclude list.
	var tunServerIPs []netip.Addr
	if cfg.tun.enable {
		ips, err := resolveServerIPs(ctx, cfg.snellCfg.Server)
		if err != nil {
			logger.Error("resolve snell server for tun mode", "server", cfg.snellCfg.Server, "err", err)
			os.Exit(1)
		}
		if len(ips) == 0 {
			logger.Error("snell server resolved to zero addresses", "server", cfg.snellCfg.Server)
			os.Exit(1)
		}
		tunServerIPs = ips
		if rewritten, ok := rewriteServerToIP(cfg.snellCfg.Server, ips[0]); ok {
			logger.Info("tun mode: pinned snell server IP", "from", cfg.snellCfg.Server, "to", rewritten)
			cfg.snellCfg.Server = rewritten
		}
	}

	client, err := snell.NewClient(cfg.snellCfg, logger)
	if err != nil {
		logger.Error("init client failed", "err", err)
		os.Exit(1)
	}

	// Decide what inbounds to run. SOCKS5 is on unless explicitly off
	// (listen = "" / "off" / "none"). TUN is on only when [snell-tun]
	// enable = true (or --tun flag). At least one must be enabled.
	socksOn := socks5Enabled(cfg.listen)
	if !socksOn && !cfg.tun.enable {
		logger.Error("no inbound enabled: set [snell-client] listen= or [snell-tun] enable=true")
		os.Exit(1)
	}

	var (
		wg      sync.WaitGroup
		firstMu sync.Mutex
		firstEr error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		firstMu.Lock()
		if firstEr == nil {
			firstEr = err
		}
		firstMu.Unlock()
		cancel()
	}

	if socksOn {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.ServeSOCKS5(ctx, cfg.listen); err != nil {
				recordErr(fmt.Errorf("socks5: %w", err))
			}
		}()
	}

	if cfg.tun.enable {
		inbound, err := startTUN(ctx, cfg, tunServerIPs, client, logger)
		if err != nil {
			recordErr(fmt.Errorf("tun: %w", err))
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-ctx.Done()
				if err := inbound.Close(); err != nil {
					logger.Warn("tun close", "err", err)
				}
			}()
		}
	}

	wg.Wait()
	if firstEr != nil {
		logger.Error("client exited", "err", firstEr)
		os.Exit(1)
	}
}

// startTUN brings up the TUN inbound. The server IP(s) were resolved
// earlier (before snell.NewClient) and are passed in so sing-tun's
// auto-route can exclude them.
func startTUN(ctx context.Context, cfg clientFileConfig, serverIPs []netip.Addr, client *snell.Client, logger *slog.Logger) (tun.Inbound, error) {
	prefix, err := netip.ParsePrefix(cfg.tun.address)
	if err != nil {
		return nil, fmt.Errorf("invalid tun address %q: %w", cfg.tun.address, err)
	}

	return tun.New(ctx, tun.Config{
		Name:      cfg.tun.iface,
		Address:   prefix,
		MTU:       cfg.tun.mtu,
		AutoRoute: cfg.tun.autoRoute,
		ServerIPs: serverIPs,
	}, client, logger)
}

// rewriteServerToIP replaces a "host:port" form with "ip:port", returning
// (rewritten, true) when the host part is not already an IP literal.
func rewriteServerToIP(server string, ip netip.Addr) (string, bool) {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return server, false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return server, false
	}
	return net.JoinHostPort(ip.String(), port), true
}

func resolveServerIPs(ctx context.Context, server string) ([]netip.Addr, error) {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	return out, nil
}

func socks5Enabled(listen string) bool {
	switch strings.ToLower(strings.TrimSpace(listen)) {
	case "", "off", "none", "disabled":
		return false
	}
	return true
}

func loadClientConfig(path string) (clientFileConfig, error) {
	f, err := ini.Load(path)
	if err != nil {
		return clientFileConfig{}, err
	}
	sec, err := f.GetSection("snell-client")
	if err != nil {
		return clientFileConfig{}, fmt.Errorf("missing [snell-client] section: %w", err)
	}

	out := clientFileConfig{
		listen: sec.Key("listen").MustString("127.0.0.1:1080"),
		snellCfg: snell.ClientConfig{
			Server:         sec.Key("server").MustString(""),
			PSK:            sec.Key("psk").MustString(""),
			ObfsMode:       sec.Key("obfs").MustString("off"),
			ObfsHost:       sec.Key("obfs-host").MustString(""),
			Reuse:          sec.Key("reuse").MustBool(false),
			Version:        sec.Key("version").MustString(""),
			TFO:            sec.Key("tfo").MustBool(false),
			Brutal:         sec.Key("brutal").MustBool(false),
			BrutalMbps:     sec.Key("brutal-mbps").MustInt(0),
			BrutalCwndGain: sec.Key("brutal-cwnd-gain").MustInt(0),
		},
	}

	// Always start from defaults so that --tun on the command line works
	// even when the config file has no [snell-tun] section at all.
	out.tun = tunFileConfig{
		address:   "198.18.0.1/16",
		mtu:       9000,
		autoRoute: true,
	}
	if tunSec, err := f.GetSection("snell-tun"); err == nil {
		out.tun = tunFileConfig{
			enable:    tunSec.Key("enable").MustBool(false),
			iface:     tunSec.Key("interface").MustString(""),
			address:   tunSec.Key("address").MustString("198.18.0.1/16"),
			mtu:       uint32(tunSec.Key("mtu").MustInt(9000)),
			autoRoute: tunSec.Key("auto-route").MustBool(true),
		}
	}

	return out, nil
}

// Compile-time check: snell.Client must satisfy tun.Dialer.
var _ tun.Dialer = (*snell.Client)(nil)
