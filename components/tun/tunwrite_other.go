/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build !linux && !darwin

package tun

import (
	"errors"

	singtun "github.com/sagernet/sing-tun"
)

// writeTunPacket is a stub on unsupported platforms — there is no
// TUN inbound to inject into, so callers should not reach here.
// Returns an error rather than panicking so a misrouted call
// surfaces in logs instead of crashing the process.
func writeTunPacket(_ singtun.Tun, _ []byte) error {
	return errors.New("tun write: not supported on this platform")
}
