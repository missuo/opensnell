/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build !linux

package snell

import "syscall"

// SO_MARK is a Linux-only socket option. The TUN auto-redirect feature
// is only available on Linux too, so a non-zero mark in client config
// can never reach this code path with the feature actually doing
// anything — the no-op below is just for cross-compile cleanliness.
func applyMarkDial(_ uint32) func(network, addr string, c syscall.RawConn) error {
	return nil
}
