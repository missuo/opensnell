/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"encoding/binary"
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// TCP Brutal (apernet/tcp-brutal) is a Linux kernel module that exposes a
// "brutal" TCP congestion-control algorithm. Userspace sets a fixed
// sending rate per-socket and the kernel will pace TCP to that rate,
// ignoring loss-based feedback. Useful on high-loss long-fat paths.
//
// Wire-protocol compatibility: brutal does NOT alter the TCP protocol
// itself — it only changes how the kernel paces outgoing segments — so a
// brutal-enabled peer can talk to a non-brutal peer without any
// incompatibility. Each side's brutal setting controls only what THAT
// side sends.
//
// Caveats:
//   1. The brutal kernel module must be loaded on the host running this
//      code. See https://github.com/apernet/tcp-brutal for installation.
//      Without it, setsockopt(TCP_CONGESTION, "brutal") fails with ENOENT
//      and we just log and continue with default congestion control.
//   2. The rate applies PER TCP CONNECTION. Snell has no native
//      multiplexing — multiple concurrent SOCKS5 / pool conns each get
//      the full configured rate, which can overwhelm the receiver.
//   3. Linux kernel ≥ 5.8 recommended; older kernels lack tcpv6_prot
//      symbol and only IPv4 brutal works. Kernel < 4.13 requires manual
//      fq pacing on the egress interface.

// tcpBrutalParams is the kernel-ABI struct expected by TCP_BRUTAL_PARAMS:
//
//	struct brutal_params {
//	    __u64 rate;       // bytes/sec
//	    __u32 cwnd_gain;  // tenths (15 = 1.5x; 20 = 2.0x)
//	} __packed;
//
// Size is 12 bytes (8 + 4) because of the __packed attribute on the
// kernel side — we must serialize it ourselves rather than relying on
// Go's alignment.
const (
	tcpCongestion    = unix.TCP_CONGESTION // 13
	tcpBrutalParams  = 23301               // not in unix package; defined by the brutal module
	brutalParamsSize = 12
)

// applyBrutal switches a TCP socket to the "brutal" congestion-control
// algorithm and sets its send-rate parameters. Should be called once
// per fresh TCP connection. Returns nil on success, an error if any
// setsockopt step fails (typically because the brutal kernel module
// isn't loaded).
func applyBrutal(c net.Conn, rateBytesPerSec uint64, cwndGain uint32) error {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		// Could be an obfs/loopback wrapper. Try walking down via a
		// net.Conn that exposes the underlying raw socket.
		return errors.New("tcp-brutal: conn is not *net.TCPConn")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return err
	}

	var sockErr error
	if cerr := raw.Control(func(fd uintptr) {
		// 1. Pick the brutal CC algorithm. SOL is IPPROTO_TCP; the value
		//    is a NUL-terminated ASCII string ("brutal"). The kernel
		//    rejects unknown algorithms with ENOENT.
		if sockErr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, tcpCongestion, "brutal"); sockErr != nil {
			return
		}
		// 2. Set the per-connection rate + cwnd gain. The kernel reads
		//    exactly 12 bytes (struct __packed); send a fixed-size slice
		//    and let SetsockoptString carry it over verbatim.
		buf := make([]byte, brutalParamsSize)
		binary.LittleEndian.PutUint64(buf[0:8], rateBytesPerSec)
		binary.LittleEndian.PutUint32(buf[8:12], cwndGain)
		sockErr = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, tcpBrutalParams, string(buf))
	}); cerr != nil {
		return cerr
	}
	return sockErr
}

// brutalSupported is always true on Linux (the actual support depends on
// the kernel module being loaded; we only know that at apply time).
func brutalSupported() bool { return true }
