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

// DefaultFakeIPPrefix is the IPv4 synthetic-IP pool's address range.
// Chosen inside RFC 2544's `198.18.0.0/15` benchmark-reserved space
// (so it never escapes to the public internet) but avoiding the
// `198.18.0.0/16` and `198.18.0.0/15` defaults of clash and sing-box
// — both of which take the lower half of 2544 — so opensnell's TUN
// can run on the same host as those tools without IP collision.
var DefaultFakeIPPrefix = netip.MustParsePrefix("198.18.128.0/17")

// Config configures the TUN inbound.
type Config struct {
	// FakeIPPrefix is the IPv4 CIDR fake-IP addresses are allocated
	// from. Defaults to DefaultFakeIPPrefix. Must be IPv4 and large
	// enough to hold a reasonable mapping cache (recommend /20 or
	// larger).
	//
	// IPv4 only by design: snell servers are almost universally v4
	// reachable, and forcing all proxied traffic through v4 sidesteps
	// the case where a v6-capable host's DNS path bypasses our
	// fake-IP layer and hands the app a real v6 address that the v4-
	// only snell server can't reach. AAAA queries get an empty
	// NOERROR reply so resolvers fall back to A.
	FakeIPPrefix netip.Prefix

	// TUNName is the kernel interface name. Empty picks "snell0" on
	// Linux; on macOS the name must be "utunN" and is auto-picked
	// when empty.
	TUNName string

	// MTU defaults to 9000 on Linux, 1500 on macOS (utun on macOS
	// caps at the system MTU minus a small overhead — keeping it at
	// 1500 avoids fragmentation surprises).
	MTU uint32

	// ServerIPs are the resolved IPv4 addresses of the snell server.
	// Used on macOS as Inet4RouteExcludeAddress so the auto-routed
	// TUN does not capture our own outbound connection to the snell
	// server (which would loop). Ignored on Linux (SO_MARK on the
	// snell-client dialer handles the bypass instead).
	//
	// main() is expected to resolve once at startup and ALSO pin the
	// resolved IP into ClientConfig.Server so subsequent re-resolves
	// (which would hit our fake-IP DNS once TUN is up) don't loop.
	ServerIPs []netip.Addr

	// ExcludeUIDs lists Linux UIDs whose outbound TCP should NOT be
	// redirected. Typical use: transparent forwarders (realm, gost)
	// running as their own non-root user. Ignored on macOS (no
	// equivalent kernel hook).
	ExcludeUIDs []uint32

	// OutputMark is the SO_MARK snell-client sets on its own outbound
	// sockets so nftables can bypass them on Linux. Defaults to
	// DefaultOutputMark. Ignored on macOS.
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
