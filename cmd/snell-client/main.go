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
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/ini.v1"

	"github.com/missuo/opensnell/components/snell"
)

type clientFileConfig struct {
	listen   string
	snellCfg snell.ClientConfig
}

func main() {
	var (
		configPath string
		verbose    bool
	)
	flag.StringVar(&configPath, "c", "/etc/snell-server/snell-client.conf", "path to ini config file")
	flag.BoolVar(&verbose, "v", false, "enable verbose logging")
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

	client, err := snell.NewClient(cfg.snellCfg, logger)
	if err != nil {
		logger.Error("init client failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := client.ServeSOCKS5(ctx, cfg.listen); err != nil {
		logger.Error("client exited", "err", err)
		os.Exit(1)
	}
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
	return out, nil
}
