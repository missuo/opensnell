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
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"strconv"
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
	enable       bool
	tunName      string
	mtu          uint32
	fakeIPPrefix netip.Prefix
	excludeUIDs  []uint32
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

	// In TUN mode the snell client's own outbound to the snell server
	// must be tagged with SO_MARK so the auto-redirect nftables rules
	// can match it and bypass the redirect. Without this, the
	// snell-client→snell-server connection would itself be captured by
	// our own listener and loop.
	if cfg.tun.enable {
		cfg.snellCfg.SocketMark = tun.DefaultOutputMark
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
			TUNName:      cfg.tun.tunName,
			MTU:          cfg.tun.mtu,
			FakeIPPrefix: cfg.tun.fakeIPPrefix,
			ExcludeUIDs:  cfg.tun.excludeUIDs,
			OutputMark:   tun.DefaultOutputMark,
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
		out.tun = tunFileConfig{
			enable:       tunSec.Key("enable").MustBool(false),
			tunName:      tunSec.Key("interface").MustString(""),
			mtu:          uint32(tunSec.Key("mtu").MustInt(0)),
			fakeIPPrefix: fakeIPPrefix,
			excludeUIDs:  uids,
		}
	}

	return out, nil
}

// Compile-time check: snell.Client must satisfy tun.Dialer.
var _ tun.Dialer = (*snell.Client)(nil)
