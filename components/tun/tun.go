/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

// Package tun is the snell-client transparent TCP inbound. Linux only.
//
// v1 uses sagernet/sing-tun's auto-redirect mode, which is a kernel-level
// nftables REDIRECT + userspace SO_ORIGINAL_DST listener. There is no
// actual TUN device involved on the data path. The two key properties
// this gives us — and the reason we chose this over the auto-route /
// default-route-hijack path:
//
//  1. PREROUTING rules explicitly exclude packets destined to a local
//     address, so inbound services on the host (sshd, nginx, caddy,
//     anything listening on a local IP) are untouched.
//  2. OUTPUT rules bypass packets that carry our own SO_MARK so the
//     snell client's connection to the snell server does not loop back
//     into its own redirect. The mark is also saved to conntrack so
//     reply traffic on an inbound flow (e.g. sshd → SSH client) also
//     bypasses the redirect, which is what keeps existing SSH sessions
//     alive and lets new SSH sessions from any source IP connect.
//
// UDP, ICMP, and traffic that needs to be transparently captured
// without a userspace TCP listener (e.g. for hostname-preserving
// AtypDomainName) are deferred to a future iteration that also brings
// up a real TUN device + userspace stack.
package tun

import (
	"context"
	"errors"
	"net"
)

// DefaultOutputMark is the fwmark snell-client stamps on its own
// outbound TCP sockets so the auto-redirect nftables rules bypass them.
// Matches sing-tun's DefaultAutoRedirectOutputMark (0x2024). Exposed so
// other packages (specifically components/snell) can apply the same
// mark via setsockopt without depending on sing-tun directly.
const DefaultOutputMark uint32 = 0x2024

// Config configures the TUN inbound.
type Config struct {
	// ExcludeUIDs lists Linux UIDs whose outbound traffic should NOT be
	// redirected through snell. Typical use: transparent TCP forwarders
	// (realm, gost, socat) need their outbound to keep going direct so
	// they continue to act as transparent forwarders, not as snell
	// clients. Empty means "no UID-level exemptions".
	ExcludeUIDs []uint32

	// OutputMark is the SO_MARK value snell-client sets on its own
	// outbound TCP sockets so nftables rules can bypass them. Defaults
	// to DefaultOutputMark when zero.
	OutputMark uint32
}

// Dialer is the subset of snell.Client the TUN inbound needs.
type Dialer interface {
	DialTCP(ctx context.Context, host string, port uint16) (net.Conn, error)
}

// Inbound is the running TUN forwarder. Close tears down the nftables
// rules and the userspace TCP listener.
type Inbound interface {
	Close() error
}

// ErrUnsupported is returned by New on non-linux platforms.
var ErrUnsupported = errors.New("snell-tun: TUN inbound is only supported on linux")
