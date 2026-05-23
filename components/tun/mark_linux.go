/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build linux

package tun

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// markDialControl returns a net.Dialer.Control hook that sets SO_MARK
// on the about-to-connect socket. Used by the directDNS forwarder so
// its UDP queries to the upstream resolver carry the same fwmark
// snell-client stamps on its own outbound — sing-tun's nftables
// exclude rule then short-circuits the DNS-hijack chain on this
// packet, letting it leave via the host's normal default route.
//
// mark == 0 returns nil so callers can pass the hook unconditionally
// without an extra nil-guard.
func markDialControl(mark uint32) func(network, addr string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(_ string, _ string, c syscall.RawConn) error {
		var sockErr error
		if cerr := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
		}); cerr != nil {
			return cerr
		}
		return sockErr
	}
}
