/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"net/netip"
	"time"
)

// reaperInterval is how often platform BypassManager implementations
// sweep expired dynamic entries. Short enough that a TTL=60s DNS
// answer doesn't keep the IP in the bypass set for long after
// expiry, long enough that we don't spam route / nftables updates
// on idle traffic.
const reaperInterval = 30 * time.Second

// BypassManager is the platform-agnostic abstraction the TUN inbound
// uses to maintain the set of destinations that should bypass the
// proxy entirely (handled by the host's normal routing).
//
// Two scopes:
//
//   - Static CIDRs added at startup (the fake-IP pool itself, plus
//     user-configured Direct IP prefixes). These never expire.
//   - Dynamic per-IP entries with a TTL — added at runtime by the
//     DNS forwarder when it resolves a Direct Domain. Re-adding an
//     IP refreshes its expiry; the implementation reaps expired
//     entries on its own schedule.
//
// Implementations are platform-specific. Today the only real impl is
// Linux (sing-tun nftables AutoRedirect's RouteExcludeAddressSet);
// macOS will add one when the GUI/helper-tool story lands. The
// abstraction lets DNS-layer code (forward.go etc.) call the same
// API regardless of platform.
type BypassManager interface {
	// AddCIDR adds a static CIDR. Permanent — never expires. Safe to
	// call concurrently.
	AddCIDR(p netip.Prefix) error

	// AddIP adds a single IP that expires after ttl. Subsequent
	// AddIP calls for the same IP refresh the expiry. ttl<=0 means
	// "permanent for the lifetime of the process" (treated like a /32
	// or /128 static CIDR internally).
	AddIP(ip netip.Addr, ttl time.Duration) error

	// Close stops internal goroutines (TTL reaper, etc) and releases
	// any platform resources held. Idempotent.
	Close() error
}
