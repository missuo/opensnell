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

// dnsServer is the tiny UDP-only DNS responder that hands out fake-IP
// answers from a FakePool. It listens on a single (IP, 53) pair —
// typically the TUN gateway address — and handles A / AAAA queries
// in-process. Everything else returns SERVFAIL.
//
// We deliberately do not forward to an upstream resolver for non-A/
// AAAA queries. The whole point of the TUN inbound is "don't trust
// the host's DNS path", so forwarding MX/TXT/SOA/PTR to a possibly
// poisoned resolver would defeat the purpose. Apps that need those
// record types in TUN mode would need a v2 design with an in-snell
// upstream forwarder.
type dnsServer struct {
	pool   *FakePool
	log    *slog.Logger
	conn   *net.UDPConn
	wg     sync.WaitGroup
	closed atomic.Bool
}

func newDNSServer(addr netip.AddrPort, pool *FakePool, log *slog.Logger) (*dnsServer, error) {
	if !addr.IsValid() {
		return nil, errors.New("dns: invalid listen address")
	}
	if pool == nil {
		return nil, errors.New("dns: pool is required")
	}
	udpAddr := net.UDPAddrFromAddrPort(addr)
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dns: listen %s: %w", addr, err)
	}
	return &dnsServer{
		pool: pool,
		log:  log,
		conn: conn,
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
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		s.log.Debug("dns parse header", "src", src, "err", err)
		return
	}
	if hdr.Response {
		// We never expect to see responses on our listen socket.
		return
	}

	// Read the question(s). DNS messages may carry several, but in
	// practice both libc and Go's resolver ship a single one per
	// message — we follow the same convention.
	q, err := p.Question()
	if err != nil {
		s.log.Debug("dns parse question", "src", src, "err", err)
		_ = s.replyFailure(hdr.ID, dnsmessage.RCodeFormatError, src)
		return
	}

	host := q.Name.String()
	switch q.Type {
	case dnsmessage.TypeA:
		s.replyA(hdr.ID, q, host, src)
	case dnsmessage.TypeAAAA:
		// IPv4-only for v1. Returning NOERROR with no answers is the
		// canonical way to say "name exists, no record of this type";
		// glibc/musl/go-resolver will then fall back to A.
		s.replyEmpty(hdr.ID, q, src)
	default:
		// CNAME / MX / TXT / PTR / SOA / SRV / SVCB etc. — see the
		// comment on dnsServer for why we don't forward these. Apps
		// that strictly need them in TUN mode are not supported by
		// v1.
		_ = s.replyFailure(hdr.ID, dnsmessage.RCodeServerFailure, src)
	}
}

func (s *dnsServer) replyA(id uint16, q dnsmessage.Question, host string, src netip.AddrPort) {
	ip, err := s.pool.Allocate(host)
	if err != nil {
		s.log.Warn("dns: fake-ip pool allocate failed", "host", host, "err", err)
		_ = s.replyFailure(id, dnsmessage.RCodeServerFailure, src)
		return
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            id,
		Response:      true,
		OpCode:        0,
		Authoritative: true,
		RecursionDesired:   true,
		RecursionAvailable: true,
		RCode:         dnsmessage.RCodeSuccess,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		s.log.Debug("dns: build questions", "err", err)
		return
	}
	if err := b.Question(q); err != nil {
		s.log.Debug("dns: write question", "err", err)
		return
	}
	if err := b.StartAnswers(); err != nil {
		s.log.Debug("dns: start answers", "err", err)
		return
	}
	if err := b.AResource(dnsmessage.ResourceHeader{
		Name:  q.Name,
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
		TTL:   fakeIPTTL,
	}, dnsmessage.AResource{A: ip.As4()}); err != nil {
		s.log.Debug("dns: write A", "err", err)
		return
	}
	msg, err := b.Finish()
	if err != nil {
		s.log.Debug("dns: finish", "err", err)
		return
	}
	if _, err := s.conn.WriteToUDPAddrPort(msg, src); err != nil {
		s.log.Debug("dns: write reply", "err", err)
		return
	}
	s.log.Debug("dns A", "q", host, "fake", ip.String(), "pool_size", s.pool.Len())
}

func (s *dnsServer) replyEmpty(id uint16, q dnsmessage.Question, src netip.AddrPort) {
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
	msg, err := b.Finish()
	if err != nil {
		return
	}
	_, _ = s.conn.WriteToUDPAddrPort(msg, src)
}

func (s *dnsServer) replyFailure(id uint16, rcode dnsmessage.RCode, src netip.AddrPort) error {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:       id,
		Response: true,
		RCode:    rcode,
	})
	msg, err := b.Finish()
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDPAddrPort(msg, src)
	return err
}
