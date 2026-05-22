/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build linux

package snell

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// applyMarkDial returns a net.Dialer.Control hook that stamps SO_MARK
// on the about-to-connect socket. Used by the TUN auto-redirect mode
// so snell-client's own outbound traffic to the snell server can be
// matched by an nftables rule and bypass the redirect — otherwise the
// connection would loop straight back into our own redirect listener.
//
// mark == 0 means "no SO_MARK"; the returned hook is a no-op so callers
// don't need to special-case the disabled path.
func applyMarkDial(mark uint32) func(network, addr string, c syscall.RawConn) error {
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
