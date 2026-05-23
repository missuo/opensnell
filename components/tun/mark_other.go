/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build !linux

package tun

import "syscall"

// markDialControl is a no-op on non-Linux platforms: SO_MARK is a
// Linux-only socket option, and the dns-hijack-bypass plumbing it
// works around only exists on the Linux TUN inbound.
func markDialControl(_ uint32) func(network, addr string, c syscall.RawConn) error {
	return nil
}
