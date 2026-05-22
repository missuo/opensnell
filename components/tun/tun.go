/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

// Package tun is the snell-client TUN inbound. Linux only.
//
// It brings up a TUN device, runs a userspace TCP/IP stack on top of it
// (via sagernet/sing-tun's "system" stack), and forwards each accepted
// flow through a snell.Client to the upstream snell server.
//
// DNS is passed through transparently: the kernel resolves names locally,
// then sends raw IP packets at the TUN — the snell server only ever sees
// destination IPs (AtypIPv4/v6). A future iteration can add a fake-IP
// resolver to preserve hostnames for AtypDomainName.
package tun

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

// Config configures the TUN inbound. Zero values pick sensible defaults
// where possible. ServerIP is required so the auto-route can carve out
// the snell server, otherwise the upstream connection would loop back
// through the TUN.
type Config struct {
	// Name is the interface name (e.g. "tun0"). Empty picks the next
	// free "tunN".
	Name string

	// Address is the TUN's own IPv4 prefix (e.g. 198.18.0.1/16). The
	// /16 default keeps the inside-TUN subnet well clear of common
	// RFC1918 ranges.
	Address netip.Prefix

	// MTU defaults to 9000 (gvisor / system stack friendly).
	MTU uint32

	// AutoRoute installs a default route + ip rule pointing into the
	// TUN. ServerIP is automatically excluded.
	AutoRoute bool

	// ServerIPs are resolved IPs of the upstream snell server. Each
	// gets a /32 (or /128) exclusion in the auto-route so client-side
	// outbound traffic to the server doesn't loop through TUN.
	ServerIPs []netip.Addr
}

// Dialer is the subset of snell.Client the TUN inbound needs. Decoupling
// keeps the components/tun package free of an import cycle with
// components/snell.
type Dialer interface {
	DialTCP(ctx context.Context, host string, port uint16) (net.Conn, error)
	DialUDP(ctx context.Context) (net.PacketConn, error)
}

// Inbound is the running TUN forwarder. Close tears everything down,
// including the kernel routes/rules sing-tun installed.
type Inbound interface {
	Close() error
}

// ErrUnsupported is returned by New on non-linux platforms.
var ErrUnsupported = errors.New("snell-tun: TUN inbound is only supported on linux")
