/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build darwin

package tun

import (
	"errors"

	singtun "github.com/sagernet/sing-tun"
	"golang.org/x/sys/unix"
)

// writeTunPacket injects a raw IP packet into the utun device.
// macOS utun requires a 4-byte address-family prefix (AF_INET /
// AF_INET6) before each IP packet — sing-tun's NativeTun.Write does
// NOT add this on darwin (unlike its Linux counterpart, which hides
// virtio framing). We prepend it ourselves.
func writeTunPacket(dev singtun.Tun, packet []byte) error {
	if len(packet) == 0 {
		return errors.New("tun write: empty packet")
	}
	var family byte
	switch packet[0] >> 4 {
	case 4:
		family = unix.AF_INET
	case 6:
		family = unix.AF_INET6
	default:
		return errors.New("tun write: unknown IP version")
	}
	buf := make([]byte, 4+len(packet))
	buf[3] = family
	copy(buf[4:], packet)
	_, err := dev.Write(buf)
	return err
}
