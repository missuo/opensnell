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
	"sync"
	"sync/atomic"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeIPTTL is the TTL we report on every A record we hand out. Keep
// it short enough that clients re-resolve relatively soon (so an
// LRU-evicted mapping doesn't strand a long-lived cache entry) but
// long enough that a single shell command doesn't trigger one query
// per HTTP request.
const fakeIPTTL = 60

// FakePools bundles the per-family fake-IP pools used by the DNS
// responder. The v6 pool may be nil when the host has no IPv6
// fake-IP coverage configured — AAAA queries then get an empty
// answer and the app falls back to A.
type FakePools struct {
	V4 *FakePool
	V6 *FakePool // may be nil
}

// Lookup walks both pools to recover the hostname for ip. The IPv4
// pool is tried first because v4 reverse-lookups are the common case
// (TCP destinations the TUN catches are usually v4).
func (p *FakePools) Lookup(ip netip.Addr) (string, bool) {
	if p == nil {
		return "", false
	}
	if p.V4 != nil && p.V4.Contains(ip) {
		return p.V4.Lookup(ip)
	}
	if p.V6 != nil && p.V6.Contains(ip) {
		return p.V6.Lookup(ip)
	}
	return "", false
}

// Contains reports whether ip falls inside any of the configured
// pools' prefixes.
func (p *FakePools) Contains(ip netip.Addr) bool {
	if p == nil {
		return false
	}
	if p.V4 != nil && p.V4.Contains(ip) {
		return true
	}
	if p.V6 != nil && p.V6.Contains(ip) {
		return true
	}
	return false
}

// ServeDNSQuery is the platform-neutral DNS query handler. Given raw
// DNS message bytes, it returns the bytes of the response to send
// back. ok=false means "no usable reply to send" (parse failure with
// no recoverable ID, etc.) — caller should drop the packet without
// writing anything.
//
// Behavior:
//
//   * A queries     → fake-IPv4 from pools.V4 (if configured), else SERVFAIL
//   * AAAA queries  → fake-IPv6 from pools.V6 (if configured), else NOERROR
//                     with no answers (so apps fall back to A)
//   * Everything else → SERVFAIL (we deliberately do NOT forward
//     MX/TXT/SOA/PTR/SRV to any upstream — the whole point of TUN
//     mode is to not trust the host's DNS path; forwarding to a
//     possibly poisoned upstream would defeat that)
//
// Used by both the Linux UDP server (which listens on the TUN
// gateway address) and the macOS PacketConn intercept (which catches
// every UDP-port-53 flow that the TUN system-stack delivers).
func ServeDNSQuery(query []byte, pools *FakePools, log *slog.Logger) (response []byte, ok bool) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		log.Debug("dns parse header", "err", err)
		return nil, false
	}
	if hdr.Response {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		log.Debug("dns parse question", "err", err)
		resp, err := buildFailure(hdr.ID, dnsmessage.RCodeFormatError)
		return resp, err == nil
	}
	switch q.Type {
	case dnsmessage.TypeA:
		if pools == nil || pools.V4 == nil {
			resp, err := buildFailure(hdr.ID, dnsmessage.RCodeServerFailure)
			return resp, err == nil
		}
		resp, err := buildAReply(hdr.ID, q, pools.V4, log)
		return resp, err == nil
	case dnsmessage.TypeAAAA:
		if pools == nil || pools.V6 == nil {
			// No v6 fake-IP pool: return NOERROR with no answers so
			// the resolver falls back to A.
			resp, err := buildEmpty(hdr.ID, q)
			return resp, err == nil
		}
		resp, err := buildAAAAReply(hdr.ID, q, pools.V6, log)
		return resp, err == nil
	default:
		resp, err := buildFailure(hdr.ID, dnsmessage.RCodeServerFailure)
		return resp, err == nil
	}
}

func buildAReply(id uint16, q dnsmessage.Question, pool *FakePool, log *slog.Logger) ([]byte, error) {
	host := q.Name.String()
	ip, err := pool.Allocate(host)
	if err != nil {
		log.Warn("dns: fake-ip v4 pool allocate failed", "host", host, "err", err)
		return buildFailure(id, dnsmessage.RCodeServerFailure)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	if err := b.AResource(dnsmessage.ResourceHeader{
		Name:  q.Name,
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
		TTL:   fakeIPTTL,
	}, dnsmessage.AResource{A: ip.As4()}); err != nil {
		return nil, err
	}
	out, err := b.Finish()
	if err != nil {
		return nil, err
	}
	log.Debug("dns A", "q", host, "fake", ip.String(), "pool_size", pool.Len())
	return out, nil
}

func buildAAAAReply(id uint16, q dnsmessage.Question, pool *FakePool, log *slog.Logger) ([]byte, error) {
	host := q.Name.String()
	ip, err := pool.Allocate(host)
	if err != nil {
		log.Warn("dns: fake-ip v6 pool allocate failed", "host", host, "err", err)
		return buildFailure(id, dnsmessage.RCodeServerFailure)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:              dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	if err := b.AAAAResource(dnsmessage.ResourceHeader{
		Name:  q.Name,
		Type:  dnsmessage.TypeAAAA,
		Class: dnsmessage.ClassINET,
		TTL:   fakeIPTTL,
	}, dnsmessage.AAAAResource{AAAA: ip.As16()}); err != nil {
		return nil, err
	}
	out, err := b.Finish()
	if err != nil {
		return nil, err
	}
	log.Debug("dns AAAA", "q", host, "fake", ip.String(), "pool_size", pool.Len())
	return out, nil
}

func buildEmpty(id uint16, q dnsmessage.Question) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            id,
		Response:      true,
		Authoritative: true,
		RCode:         dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	return b.Finish()
}

func buildFailure(id uint16, rcode dnsmessage.RCode) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:       id,
		Response: true,
		RCode:    rcode,
	})
	return b.Finish()
}

// dnsServer is the tiny UDP-only DNS responder that hands out fake-IP
// answers. It listens on a single (IP, 53) pair — typically the TUN
// gateway address — and dispatches each query to ServeDNSQuery. Used
// by the Linux TUN inbound; the macOS path inlines ServeDNSQuery
// into the sing-tun UDP NAT callback instead.
type dnsServer struct {
	pools  *FakePools
	log    *slog.Logger
	conn   *net.UDPConn
	wg     sync.WaitGroup
	closed atomic.Bool
}

func newDNSServer(addr netip.AddrPort, pools *FakePools, log *slog.Logger) (*dnsServer, error) {
	if !addr.IsValid() {
		return nil, errors.New("dns: invalid listen address")
	}
	if pools == nil || pools.V4 == nil {
		return nil, errors.New("dns: at least the v4 pool is required")
	}
	udpAddr := net.UDPAddrFromAddrPort(addr)
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dns: listen %s: %w", addr, err)
	}
	return &dnsServer{
		pools: pools,
		log:   log,
		conn:  conn,
	}, nil
}

func (s *dnsServer) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.loop(ctx)
}

func (s *dnsServer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := s.conn.Close()
	s.wg.Wait()
	return err
}

func (s *dnsServer) loop(ctx context.Context) {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, src, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.closed.Load() || ctx.Err() != nil {
				return
			}
			s.log.Debug("dns read", "err", err)
			continue
		}
		s.handle(buf[:n], src)
	}
}

func (s *dnsServer) handle(query []byte, src netip.AddrPort) {
	resp, ok := ServeDNSQuery(query, s.pools, s.log)
	if !ok || len(resp) == 0 {
		return
	}
	if _, err := s.conn.WriteToUDPAddrPort(resp, src); err != nil {
		s.log.Debug("dns: write reply", "err", err)
	}
}
