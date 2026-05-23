/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// directDNS forwards DNS queries for configured "direct domains" to
// an upstream resolver instead of synthesizing fake-IPs. It then
// parses the upstream's response to extract A/AAAA records and
// registers each returned IP with the BypassManager so the kernel
// stops routing matching outbound TCP through the TUN — the host
// connects to the real IP via its normal default route.
//
// This is the runtime arm of the Linux "Direct Domain" feature.
// Static "Direct IP" prefixes are handled by passing them straight
// to BypassManager.AddCIDR at startup.
type directDNS struct {
	upstream string   // "ip:port" — the resolver we forward to
	suffixes []string // lower-case, no trailing dot
	bypass   BypassManager
	dialer   net.Dialer
	timeout  time.Duration
	log      *slog.Logger
}

// upstreamTimeout caps how long we wait for the upstream resolver to
// respond. DNS over UDP usually completes in <100ms; we set the cap
// generously so a slow public DNS doesn't break the resolver path
// but cut it well below the typical client retry interval.
const upstreamTimeout = 3 * time.Second

// newDirectDNS validates inputs and returns the forwarder. Returns
// (nil, nil) when the feature is unused (no domains configured).
//
// outputMark, when non-zero, is stamped as SO_MARK on the upstream
// UDP socket so sing-tun's nftables DNS-hijack rule excludes the
// packet and lets it leave via the host's normal default route.
// Required on Linux when running under the TUN auto-redirect — the
// hijack rule would otherwise loop our query right back into the
// gateway:53 fake-IP server (which is us). Zero on darwin / when
// the hijack isn't in play.
func newDirectDNS(upstream string, domains []string, bypass BypassManager, outputMark uint32, log *slog.Logger) (*directDNS, error) {
	domains = normalizeDirectSuffixes(domains)
	if len(domains) == 0 {
		return nil, nil
	}
	if bypass == nil {
		return nil, errors.New("direct-dns: bypass manager is required")
	}
	if strings.TrimSpace(upstream) == "" {
		return nil, errors.New("direct-dns: upstream is required when direct-domains is set")
	}
	if _, _, err := net.SplitHostPort(upstream); err != nil {
		// Allow bare "1.1.1.1" — assume default DNS port.
		upstream = net.JoinHostPort(upstream, "53")
	}
	if _, _, err := net.SplitHostPort(upstream); err != nil {
		return nil, fmt.Errorf("direct-dns: invalid upstream %q: %w", upstream, err)
	}
	d := net.Dialer{Timeout: upstreamTimeout}
	d.Control = markDialControl(outputMark)
	return &directDNS{
		upstream: upstream,
		suffixes: domains,
		bypass:   bypass,
		dialer:   d,
		timeout:  upstreamTimeout,
		log:      log,
	}, nil
}

// normalizeDirectSuffixes lower-cases, strips leading/trailing dots,
// drops empties, and de-duplicates. Returns nil for empty input so
// callers can short-circuit.
func normalizeDirectSuffixes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimPrefix(s, ".")
		s = strings.TrimSuffix(s, ".")
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matches reports whether the DNS question name matches any
// configured suffix. Match is by domain-suffix: "example.com" matches
// "example.com" and "foo.example.com" but not "notexample.com".
func (d *directDNS) matches(qname string) bool {
	if d == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSuffix(qname, "."))
	for _, sfx := range d.suffixes {
		if name == sfx || strings.HasSuffix(name, "."+sfx) {
			return true
		}
	}
	return false
}

// handle forwards a raw DNS query to the upstream resolver,
// registers any returned A/AAAA addresses in the bypass set, and
// returns the upstream's response bytes verbatim. The verbatim
// passthrough is important: re-building the response would lose
// any non-standard sections the upstream populated (EDNS0 options,
// CNAME chains, etc.).
//
// On upstream failure we return ok=false so the caller falls back
// to whatever error behavior it would have used otherwise (typically
// SERVFAIL).
func (d *directDNS) handle(query []byte) ([]byte, bool) {
	dialCtx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	conn, err := d.dialer.DialContext(dialCtx, "udp", d.upstream)
	if err != nil {
		d.log.Debug("direct-dns: dial upstream failed", "upstream", d.upstream, "err", err)
		return nil, false
	}
	defer conn.Close()
	deadline := time.Now().Add(d.timeout)
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(query); err != nil {
		d.log.Debug("direct-dns: write query failed", "err", err)
		return nil, false
	}
	// 1500 = standard MTU; EDNS0 callers can use more but our use
	// case (Direct Domain DNS) typically returns a handful of A/AAAA
	// records that fit comfortably.
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		d.log.Debug("direct-dns: read response failed", "err", err)
		return nil, false
	}
	resp := buf[:n]
	d.registerAnswers(resp)
	return resp, true
}

// registerAnswers walks the response's Answer section, pulls every
// A and AAAA RDATA into the bypass set with the record's own TTL.
// Best-effort: a parse error mid-section is logged at debug and the
// rest of the records are skipped, but the upstream's bytes still
// get returned to the client (registerAnswers is called for its
// side-effect on the bypass set; failure here is non-fatal to the
// DNS reply itself).
func (d *directDNS) registerAnswers(resp []byte) {
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		d.log.Debug("direct-dns: parse response", "err", err)
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		d.log.Debug("direct-dns: skip questions", "err", err)
		return
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			// Includes io.EOF / dnsmessage.ErrSectionDone — normal
			// terminator. Anything else is malformed-but-we-don't-care.
			return
		}
		ttl := time.Duration(h.TTL) * time.Second
		if ttl <= 0 {
			ttl = 60 * time.Second
		}
		switch h.Type {
		case dnsmessage.TypeA:
			rr, err := p.AResource()
			if err != nil {
				return
			}
			ip := netip.AddrFrom4(rr.A)
			if err := d.bypass.AddIP(ip, ttl); err != nil {
				d.log.Debug("direct-dns: bypass AddIP", "ip", ip, "err", err)
			} else {
				d.log.Debug("direct-dns: bypass added", "name", h.Name.String(), "ip", ip, "ttl", ttl)
			}
		case dnsmessage.TypeAAAA:
			rr, err := p.AAAAResource()
			if err != nil {
				return
			}
			ip := netip.AddrFrom16(rr.AAAA)
			if err := d.bypass.AddIP(ip, ttl); err != nil {
				d.log.Debug("direct-dns: bypass AddIP", "ip", ip, "err", err)
			} else {
				d.log.Debug("direct-dns: bypass added", "name", h.Name.String(), "ip", ip, "ttl", ttl)
			}
		default:
			if err := p.SkipAnswer(); err != nil {
				return
			}
		}
	}
}
