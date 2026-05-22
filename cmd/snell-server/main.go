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

func main() {
	var (
		configPath string
		verbose    bool
	)
	flag.StringVar(&configPath, "c", "/etc/snell-server/snell-server.conf", "path to ini config file")
	flag.BoolVar(&verbose, "v", false, "enable verbose logging")
	flag.Parse()

	logLevel := slog.LevelInfo
	if verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	cfg, err := loadServerConfig(configPath)
	if err != nil {
		logger.Error("load config failed", "path", configPath, "err", err)
		os.Exit(1)
	}

	srv, err := snell.NewServer(cfg, logger)
	if err != nil {
		logger.Error("init server failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func loadServerConfig(path string) (snell.ServerConfig, error) {
	f, err := ini.Load(path)
	if err != nil {
		return snell.ServerConfig{}, err
	}
	sec, err := f.GetSection("snell-server")
	if err != nil {
		return snell.ServerConfig{}, fmt.Errorf("missing [snell-server] section: %w", err)
	}

	// The official snell-server has no `version` knob — it always serves the
	// v5 backend, which is documented backward-compatible with v4 clients.
	// We mirror that: any `version` key the user puts in the config is
	// silently ignored (consistent with the official binary's behavior).
	cfg := snell.ServerConfig{
		Listen:   sec.Key("listen").MustString("0.0.0.0:8388"),
		PSK:      sec.Key("psk").MustString(""),
		ObfsMode: sec.Key("obfs").MustString("off"),
		UDP:      sec.Key("udp").MustBool(true),
	}
	return cfg, nil
}
