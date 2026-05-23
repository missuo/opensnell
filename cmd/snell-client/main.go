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
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	enable       bool
	tunName      string
	mtu          uint32
	fakeIPPrefix netip.Prefix
	excludeUIDs  []uint32

	disableIPv6       bool
	ipv6ProbeTarget   string
	ipv6ProbeInterval time.Duration

	directIPs     []netip.Prefix
	directDomains []string
	upstreamDNS   string
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

	// TUN-mode bypass for snell-client's own outbound to the snell
	// server, both forms required:
	//
	//   1. SO_MARK 0x2024 on Linux — nftables OUTPUT chain matches the
	//      mark and skips the auto-redirect, so the connection lands on
	//      the real interface instead of our own listener.
	//   2. Pinned server IP universally — when TUN is up our fake-IP
	//      DNS server intercepts every UDP :53 outbound on Linux (via
	//      nft DNAT) and every UDP packet on macOS (via the TUN system
	//      stack). If `server = host:port` is a hostname, snell-client
	//      would re-resolve it after TUN is up, get back a fake-IP,
	//      and TCP-connect to that fake-IP — which routes via the TUN
	//      and arrives at our handler, which would call DialTCP with
	//      the hostname again, looping. Resolving once before TUN
	//      starts and rewriting Server to ip:port form breaks the
	//      cycle. On macOS the resolved IP is also installed as a
	//      Inet4RouteExcludeAddress so the kernel doesn't even try to
	//      route it through the TUN.
	var tunServerIPs []netip.Addr
	if cfg.tun.enable {
		cfg.snellCfg.SocketMark = tun.DefaultOutputMark

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
		inbound, err := tun.New(ctx, tun.Config{
			TUNName:           cfg.tun.tunName,
			MTU:               cfg.tun.mtu,
			FakeIPPrefix:      cfg.tun.fakeIPPrefix,
			ServerIPs:         tunServerIPs,
			ExcludeUIDs:       cfg.tun.excludeUIDs,
			OutputMark:        tun.DefaultOutputMark,
			DisableIPv6:       cfg.tun.disableIPv6,
			IPv6ProbeTarget:   cfg.tun.ipv6ProbeTarget,
			IPv6ProbeInterval: cfg.tun.ipv6ProbeInterval,
			DirectIPs:         cfg.tun.directIPs,
			DirectDomains:     cfg.tun.directDomains,
			UpstreamDNS:       cfg.tun.upstreamDNS,
		}, client, logger)
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

// resolveServerIPs converts "host:port" to a list of resolved IPs.
// Called once before TUN starts, so the resolution happens through the
// system's normal (TUN-free) path. Returns IPv4-first; IPv6 entries
// are kept too but the TUN's auto-route exclusion (macOS) only sets
// IPv4 today.
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

// rewriteServerToIP returns ("ip:port", true) when host is a hostname,
// or (server, false) when it's already an IP literal. Lets us pin the
// resolved IP into ClientConfig.Server so the snell-client dialer
// never re-resolves through the TUN's fake-IP DNS.
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

func socks5Enabled(listen string) bool {
	switch strings.ToLower(strings.TrimSpace(listen)) {
	case "", "off", "none", "disabled":
		return false
	}
	return true
}

// parseExcludeUIDs splits a comma-separated list of UIDs / usernames and
// resolves each entry to a numeric UID. Names are looked up via the
// system user database (NSS). Empty / whitespace entries are ignored.
func parseExcludeUIDs(raw string) ([]uint32, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []uint32
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if n, err := strconv.ParseUint(item, 10, 32); err == nil {
			out = append(out, uint32(n))
			continue
		}
		u, err := user.Lookup(item)
		if err != nil {
			return nil, fmt.Errorf("exclude-uid: unknown user %q: %w", item, err)
		}
		n, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("exclude-uid: %q has non-numeric UID %q: %w", item, u.Uid, err)
		}
		out = append(out, uint32(n))
	}
	return out, nil
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

	if tunSec, err := f.GetSection("snell-tun"); err == nil {
		uids, err := parseExcludeUIDs(tunSec.Key("exclude-uid").MustString(""))
		if err != nil {
			return clientFileConfig{}, err
		}
		var fakeIPPrefix netip.Prefix
		if rawPrefix := strings.TrimSpace(tunSec.Key("fake-ip-range").MustString("")); rawPrefix != "" {
			fakeIPPrefix, err = netip.ParsePrefix(rawPrefix)
			if err != nil {
				return clientFileConfig{}, fmt.Errorf("[snell-tun] fake-ip-range %q: %w", rawPrefix, err)
			}
		}
		directIPs, err := parsePrefixList(tunSec.Key("direct-ip").MustString(""))
		if err != nil {
			return clientFileConfig{}, fmt.Errorf("[snell-tun] direct-ip: %w", err)
		}
		directDomains := splitCSV(tunSec.Key("direct-domain").MustString(""))
		// IPv6 config: the ini key "ipv6" is a positive enable
		// switch matching how Mihomo/Clash express it. Internally we
		// store the inverse (DisableIPv6) to keep the zero-value
		// path mean "enabled by default with probe".
		ipv6Enabled := tunSec.Key("ipv6").MustBool(true)

		// Probe interval accepts Go duration strings ("5m", "30s").
		// 0 / empty leaves the default in place.
		var probeInterval time.Duration
		if rawInt := strings.TrimSpace(tunSec.Key("ipv6-probe-interval").MustString("")); rawInt != "" {
			probeInterval, err = time.ParseDuration(rawInt)
			if err != nil {
				return clientFileConfig{}, fmt.Errorf("[snell-tun] ipv6-probe-interval %q: %w", rawInt, err)
			}
		}

		out.tun = tunFileConfig{
			enable:            tunSec.Key("enable").MustBool(false),
			tunName:           tunSec.Key("interface").MustString(""),
			mtu:               uint32(tunSec.Key("mtu").MustInt(0)),
			fakeIPPrefix:      fakeIPPrefix,
			excludeUIDs:       uids,
			disableIPv6:       !ipv6Enabled,
			ipv6ProbeTarget:   strings.TrimSpace(tunSec.Key("ipv6-probe-target").MustString("")),
			ipv6ProbeInterval: probeInterval,
			directIPs:         directIPs,
			directDomains:     directDomains,
			upstreamDNS:       strings.TrimSpace(tunSec.Key("upstream-dns").MustString("")),
		}
	}

	return out, nil
}

// splitCSV is the canonical "comma-separated list of strings" parser
// for the snell-client config. Trims whitespace and drops empties.
// Used for both direct-domain and any future CSV fields.
func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parsePrefixList parses "a.b.c.d/m, e.f.g.h/n" — a comma-separated
// list of CIDRs — into netip.Prefix. Bare IP literals are accepted
// and treated as /32 or /128.
func parsePrefixList(raw string) ([]netip.Prefix, error) {
	items := splitCSV(raw)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(items))
	for _, s := range items {
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", s, err)
		}
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		out = append(out, netip.PrefixFrom(addr, bits))
	}
	return out, nil
}

// Compile-time check: snell.Client must satisfy tun.Dialer.
var _ tun.Dialer = (*snell.Client)(nil)
