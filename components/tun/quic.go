/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"encoding/binary"
	"errors"
	"net/netip"

	singtun "github.com/sagernet/sing-tun"
)

// quicTriggerPort is the destination UDP port we treat as "QUIC for
// the web" and respond to with ICMP Port Unreachable. HTTP/3 is
// universally on UDP/443; other QUIC apps use varied ports and we
// deliberately do NOT trigger fallback on those — sending ICMP
// unreachable to a random UDP flow could break games / VoIP / mDNS
// in ways the user doesn't want. 443 is the conservative pick.
const quicTriggerPort uint16 = 443

// sendQUICPortUnreachable injects an ICMPv4 Destination Unreachable /
// Port Unreachable (type 3, code 3) or ICMPv6 Destination Unreachable /
// Port Unreachable (type 1, code 4) packet into the TUN device,
// addressed back to the original source. The packet carries a
// fabricated copy of the "original" UDP header (matching src/dst
// IP+port) so the kernel can deliver the error to the app's UDP
// socket via the standard ICMP-error path (Linux IP_RECVERR / macOS
// equivalent).
//
// Why: every major browser's QUIC stack waits ~10 seconds before
// abandoning QUIC and falling back to TCP/TLS when UDP/443 just
// disappears. ICMP Port Unreachable shortcuts that — the QUIC
// implementation sees the error, marks the destination as
// QUIC-broken, and Alt-Svc-invalidates immediately.
//
// Best-effort: a write failure is logged at debug level by the
// caller and the connection is dropped silently as before. We do
// not retry — if injection didn't work the first time, the issue is
// likely persistent (TUN closed, family mismatch) and a retry would
// just spam.
func sendQUICPortUnreachable(dev singtun.Tun, src, dst netip.AddrPort) error {
	if !src.IsValid() || !dst.IsValid() {
		return errors.New("quic-icmp: invalid addr")
	}
	srcAddr := src.Addr().Unmap()
	dstAddr := dst.Addr().Unmap()
	if srcAddr.Is4() != dstAddr.Is4() {
		return errors.New("quic-icmp: src/dst family mismatch")
	}
	var pkt []byte
	if srcAddr.Is4() {
		pkt = buildICMPv4PortUnreachable(srcAddr, src.Port(), dstAddr, dst.Port())
	} else {
		pkt = buildICMPv6PortUnreachable(srcAddr, src.Port(), dstAddr, dst.Port())
	}
	return writeTunPacket(dev, pkt)
}

// buildICMPv4PortUnreachable assembles an IPv4+ICMP packet headed
// back to the app (origSrc). The embedded "original packet" inside
// the ICMP body carries fabricated IPv4+UDP headers with src/dst
// swapped from this reply's perspective — i.e. matching what the
// app sent: src=origSrc, dst=origDst, sport=origSrcPort,
// dport=origDstPort. The kernel matches by those four tuple
// values when delivering the error to the app's socket.
func buildICMPv4PortUnreachable(origSrc netip.Addr, origSrcPort uint16, origDst netip.Addr, origDstPort uint16) []byte {
	// Total: 20 (outer IP) + 8 (ICMP) + 20 (inner IP) + 8 (UDP) = 56.
	pkt := make([]byte, 56)

	// Inner IP header (offset 28..47) — what the app sent.
	inner := pkt[28:]
	inner[0] = 0x45 // v4, IHL=5
	inner[1] = 0x00
	binary.BigEndian.PutUint16(inner[2:4], 28) // total len = IP + UDP (no body)
	binary.BigEndian.PutUint16(inner[4:6], 0)  // ID
	binary.BigEndian.PutUint16(inner[6:8], 0)  // flags+frag
	inner[8] = 64                              // TTL
	inner[9] = 17                              // UDP
	binary.BigEndian.PutUint16(inner[10:12], 0)
	srcB := origSrc.As4()
	dstB := origDst.As4()
	copy(inner[12:16], srcB[:])
	copy(inner[16:20], dstB[:])
	binary.BigEndian.PutUint16(inner[10:12], onesComplementChecksum(inner[:20]))

	// Inner UDP header (offset 48..55).
	udp := pkt[48:]
	binary.BigEndian.PutUint16(udp[0:2], origSrcPort)
	binary.BigEndian.PutUint16(udp[2:4], origDstPort)
	binary.BigEndian.PutUint16(udp[4:6], 8) // length = just the UDP header
	binary.BigEndian.PutUint16(udp[6:8], 0) // checksum (allowed to be 0 in v4)

	// ICMP header (offset 20..27): type=3 code=3, 4 zero bytes.
	icmp := pkt[20:28]
	icmp[0] = 3
	icmp[1] = 3
	// icmp[2:4] checksum filled below
	// icmp[4:8] unused — already zero

	// Outer IP header (offset 0..19) — the reply we're sending.
	binary.BigEndian.PutUint16(icmp[2:4], onesComplementChecksum(pkt[20:]))
	pkt[0] = 0x45
	pkt[1] = 0x00
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	binary.BigEndian.PutUint16(pkt[4:6], 0)
	binary.BigEndian.PutUint16(pkt[6:8], 0)
	pkt[8] = 64 // TTL
	pkt[9] = 1  // ICMP
	binary.BigEndian.PutUint16(pkt[10:12], 0)
	copy(pkt[12:16], dstB[:]) // outer src = origDst
	copy(pkt[16:20], srcB[:]) // outer dst = origSrc
	binary.BigEndian.PutUint16(pkt[10:12], onesComplementChecksum(pkt[:20]))

	return pkt
}

// buildICMPv6PortUnreachable assembles the IPv6 equivalent. ICMPv6
// checksums include a pseudo-header (src + dst + upper-layer length
// + next-header=58), which onesComplementChecksum handles via the
// pseudo prefix.
func buildICMPv6PortUnreachable(origSrc netip.Addr, origSrcPort uint16, origDst netip.Addr, origDstPort uint16) []byte {
	// Total: 40 (outer IPv6) + 8 (ICMPv6) + 40 (inner IPv6) + 8 (UDP) = 96.
	pkt := make([]byte, 96)
	srcB := origSrc.As16()
	dstB := origDst.As16()

	// Inner IPv6 header (offset 48..87) — what the app sent.
	inner := pkt[48:]
	inner[0] = 0x60
	// inner[1..4] zero (traffic class + flow label) — keep zero
	binary.BigEndian.PutUint16(inner[4:6], 8) // payload len = UDP header only
	inner[6] = 17                             // next header = UDP
	inner[7] = 64                             // hop limit
	copy(inner[8:24], srcB[:])
	copy(inner[24:40], dstB[:])

	// Inner UDP (offset 88..95).
	udp := pkt[88:]
	binary.BigEndian.PutUint16(udp[0:2], origSrcPort)
	binary.BigEndian.PutUint16(udp[2:4], origDstPort)
	binary.BigEndian.PutUint16(udp[4:6], 8)
	// UDP checksum is mandatory in v6 — but the ICMP container only
	// requires "as much of the original as fits" and apps that match
	// by ports don't validate the inner UDP checksum. We leave it
	// zero; the kernel's error-matching code doesn't re-verify it.
	binary.BigEndian.PutUint16(udp[6:8], 0)

	// ICMPv6 header (offset 40..47): type=1 code=4.
	icmp := pkt[40:48]
	icmp[0] = 1
	icmp[1] = 4
	// checksum (icmp[2:4]) computed below over pseudo-header + ICMP message

	// Outer IPv6 header (offset 0..39).
	pkt[0] = 0x60
	binary.BigEndian.PutUint16(pkt[4:6], uint16(len(pkt)-40)) // payload len
	pkt[6] = 58                                               // next header = ICMPv6
	pkt[7] = 64                                               // hop limit
	copy(pkt[8:24], dstB[:])                                  // outer src = origDst
	copy(pkt[24:40], srcB[:])                                 // outer dst = origSrc

	// ICMPv6 checksum: pseudo-header (src 16 + dst 16 + 4-byte BE length + 3 zeros + next-header 1) || ICMP message.
	pseudo := make([]byte, 0, 40+len(icmp)+48)
	pseudo = append(pseudo, pkt[8:24]...)  // outer src
	pseudo = append(pseudo, pkt[24:40]...) // outer dst
	var lenAndNext [8]byte
	binary.BigEndian.PutUint32(lenAndNext[0:4], uint32(len(pkt)-40))
	lenAndNext[7] = 58 // next header
	pseudo = append(pseudo, lenAndNext[:]...)
	pseudo = append(pseudo, pkt[40:]...) // ICMP message + body
	binary.BigEndian.PutUint16(icmp[2:4], onesComplementChecksum(pseudo))

	return pkt
}

// onesComplementChecksum computes the standard 16-bit one's-complement
// sum used by IPv4 / ICMP / ICMPv6 / UDP / TCP checksums.
func onesComplementChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}
