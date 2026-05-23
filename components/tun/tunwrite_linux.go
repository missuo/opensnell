/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

//go:build linux

package tun

import singtun "github.com/sagernet/sing-tun"

// writeTunPacket injects a raw IP packet into the TUN device. On
// Linux the sing-tun NativeTun.Write handles any internal framing
// (virtio-net header when GSO is on) — we pass the bare IP packet.
func writeTunPacket(dev singtun.Tun, packet []byte) error {
	_, err := dev.Write(packet)
	return err
}
