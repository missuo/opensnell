/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

// Package tun is the snell-client transparent TCP inbound. Linux only.
//
// v2 architecture (in-tree, the only mode):
//
//   App ─DNS─►  UDP 53 (sing-tun DNS hijack DNATs to gateway:53)
//                                  │
//                                  ▼
//                       snell-client fake-IP DNS server
//                                  │  (allocates 198.18.128.42 ⇄ "name")
//                                  ▼
//   App ─TCP─►  198.18.128.42:443  (kernel routes via TUN; nft redirect
//                                   excludes the fake-IP CIDR so this
//                                   stays on the TUN data path)
//                                  │
//                                  ▼
//                    sing-tun "system" userspace stack
//                                  │
//                                  ▼
//          handler reverse-lookup pool ⇒ "registry-1.docker.io"
//                                  │
//                                  ▼
//                snell.Client.DialTCP(name, port)  ← AtypDomainName
//                                  │
//                                  ▼
//             snell server uses ITS resolver, gets the real IP
//
// Everything else (TCP to real IPs that aren't fake-IPs) goes through
// sing-tun's nftables auto-redirect, which preserves established
// inbound connections, leaves replies from local services untouched,
// and routes new outbound through the same snell.Client. The two
// inbound paths share one handler.
//
// Why fake-IP at all: many users live behind DNS-poisoned resolvers
// (e.g. mainland China ISP NSes return junk IPs for registry-1.docker.io
// and similar). If the host resolves locally and only the resulting IP
// reaches the snell server, the IP is already wrong and snell can't
// help. Issuing a fake-IP and using AtypDomainName makes the snell
// server itself responsible for resolving the name through its own
// (presumably uncontaminated) resolver.
package tun

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

// DefaultOutputMark is the fwmark snell-client stamps on its own
// outbound TCP sockets so the auto-redirect nftables rules bypass them.
// Matches sing-tun's DefaultAutoRedirectOutputMark (0x2024).
const DefaultOutputMark uint32 = 0x2024

// DefaultFakeIPPrefix is the synthetic IP pool's address range. Chosen
// to be inside RFC 2544's `198.18.0.0/15` benchmark-reserved space
// (so it never escapes to the public internet) but avoiding the
// `198.18.0.0/16` and `198.18.0.0/15` defaults of clash and sing-box
// — both of which take the lower half of 2544 — so opensnell's TUN
// can run on the same host as those tools without IP collision.
var DefaultFakeIPPrefix = netip.MustParsePrefix("198.18.128.0/17")

// Config configures the TUN inbound.
type Config struct {
	// FakeIPPrefix is the CIDR fake-IP addresses are allocated from.
	// Defaults to DefaultFakeIPPrefix. Must be IPv4 and large enough
	// to hold a reasonable mapping cache (recommend /20 or larger).
	FakeIPPrefix netip.Prefix

	// TUNName is the kernel interface name. Empty picks "snell0".
	TUNName string

	// MTU defaults to 9000.
	MTU uint32

	// ExcludeUIDs lists Linux UIDs whose outbound TCP should NOT be
	// redirected. Typical use: transparent forwarders (realm, gost)
	// running as their own non-root user.
	ExcludeUIDs []uint32

	// OutputMark is the SO_MARK snell-client sets on its own outbound
	// sockets so nftables can bypass them. Defaults to DefaultOutputMark.
	OutputMark uint32
}

// Dialer is the subset of snell.Client the TUN inbound needs.
type Dialer interface {
	DialTCP(ctx context.Context, host string, port uint16) (net.Conn, error)
}

// Inbound is the running TUN forwarder. Close tears down all kernel
// rules and userspace listeners.
type Inbound interface {
	Close() error
}

// ErrUnsupported is returned by New on non-linux platforms.
var ErrUnsupported = errors.New("snell-tun: TUN inbound is only supported on linux")
