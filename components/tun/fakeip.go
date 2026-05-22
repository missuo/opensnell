/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"container/list"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"sync"
)

// FakePool maps hostnames to synthetic IP addresses within a fixed
// CIDR. The pool is family-aware: a single pool serves either IPv4
// or IPv6 depending on the prefix it was constructed with. Each
// hostname is stable inside a pool — the second allocation for the
// same hostname returns the same address. When the pool is
// exhausted, the least-recently-used hostname is evicted and its
// IP is reused.
//
// Concurrency: safe for many goroutines.
//
// Bidirectional: at allocation time the DNS server hands out an IP;
// at TCP-connect time the TUN handler needs to recover the original
// hostname so it can talk to the snell server with AtypDomainName.
// Both directions are O(1).
type FakePool struct {
	mu sync.Mutex

	prefix    netip.Prefix
	firstUsed netip.Addr // smallest IP we ever hand out
	lastUsed  netip.Addr // largest IP we ever hand out

	byHost map[string]*list.Element
	byIP   map[netip.Addr]*list.Element
	lru    *list.List // *fakeEntry, front = MRU, back = LRU
	max    int        // max entries
	next   netip.Addr // next IP to try when growing the pool
}

type fakeEntry struct {
	host string
	ip   netip.Addr
}

// NewFakePool constructs a pool over prefix. The first 16 addresses
// are reserved (network address, "gateway", and a small headroom for
// the DNS server), and the highest address (network broadcast on v4
// / all-ones suffix on v6) is reserved at the top. Reserved
// addresses are never handed out for hostname mapping.
//
// max <= 0 means "as big as the prefix allows" (capped at MaxInt32
// for sanity — a /64 v6 prefix would otherwise pretend to have 2^64
// slots, which is meaningless).
func NewFakePool(prefix netip.Prefix, max int) (*FakePool, error) {
	if !prefix.IsValid() {
		return nil, fmt.Errorf("fake-ip: invalid prefix %q", prefix)
	}
	hostBits := 32
	if prefix.Addr().Is6() && !prefix.Addr().Is4In6() {
		hostBits = 128
	}
	hostBits -= prefix.Bits()
	if hostBits < 5 {
		return nil, fmt.Errorf("fake-ip: prefix %q too small", prefix)
	}

	network := prefix.Masked().Addr()
	first := addrAdd(network, 16)
	last := addrSub(lastInPrefix(prefix), 1)
	if first.Compare(last) > 0 {
		return nil, fmt.Errorf("fake-ip: prefix %q too small after reserving headroom", prefix)
	}

	capacity := math.MaxInt32
	if hostBits <= 31 {
		// (1 << hostBits) – 17 headroom is the exact usable count
		capacity = (1 << hostBits) - 17
	}
	if max <= 0 || max > capacity {
		max = capacity
	}

	return &FakePool{
		prefix:    prefix,
		firstUsed: first,
		lastUsed:  last,
		byHost:    make(map[string]*list.Element, min(max, 1<<14)/4),
		byIP:      make(map[netip.Addr]*list.Element, min(max, 1<<14)/4),
		lru:       list.New(),
		max:       max,
		next:      first,
	}, nil
}

// Prefix returns the CIDR the pool draws from.
func (p *FakePool) Prefix() netip.Prefix { return p.prefix }

// Gateway returns the first usable address in the prefix (e.g.
// 198.18.128.1 for 198.18.128.0/17, or fdee::1 for fdee::/96). This
// is the TUN device's own address and is excluded from fake-IP
// allocations.
func (p *FakePool) Gateway() netip.Addr {
	return addrAdd(p.prefix.Masked().Addr(), 1)
}

// Allocate returns a fake IP for the given hostname. Repeated calls
// for the same hostname return the same IP (and mark it as recently
// used). When the pool is full, the least-recently-used hostname is
// evicted and its slot recycled.
//
// Hostnames are normalized to lowercase and trailing-dot-stripped so
// callers don't have to worry about DNS-message-vs-config casing.
func (p *FakePool) Allocate(hostname string) (netip.Addr, error) {
	host := normalizeHost(hostname)
	if host == "" {
		return netip.Addr{}, errors.New("fake-ip: empty hostname")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if el, ok := p.byHost[host]; ok {
		p.lru.MoveToFront(el)
		return el.Value.(*fakeEntry).ip, nil
	}

	var ip netip.Addr
	if len(p.byHost) < p.max {
		ip = p.next
		p.next = addrAdd(p.next, 1)
	} else {
		back := p.lru.Back()
		if back == nil {
			return netip.Addr{}, errors.New("fake-ip: pool full but LRU empty (impossible)")
		}
		victim := back.Value.(*fakeEntry)
		ip = victim.ip
		delete(p.byHost, victim.host)
		delete(p.byIP, victim.ip)
		p.lru.Remove(back)
	}

	entry := &fakeEntry{host: host, ip: ip}
	el := p.lru.PushFront(entry)
	p.byHost[host] = el
	p.byIP[ip] = el
	return ip, nil
}

// Lookup returns the hostname previously allocated to ip, marking
// the entry as recently used.
func (p *FakePool) Lookup(ip netip.Addr) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	el, ok := p.byIP[ip.Unmap()]
	if !ok {
		return "", false
	}
	p.lru.MoveToFront(el)
	return el.Value.(*fakeEntry).host, true
}

// Contains reports whether ip falls inside the pool's prefix.
func (p *FakePool) Contains(ip netip.Addr) bool {
	return p.prefix.Contains(ip.Unmap())
}

// Len returns the number of currently-mapped hostnames.
func (p *FakePool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byHost)
}

// ----- IP arithmetic helpers, family-aware -----

// addrAdd adds n to the address, treating the bytes as big-endian.
// Works for both v4 (4-byte) and v6 (16-byte) addresses.
func addrAdd(a netip.Addr, n uint32) netip.Addr {
	if a.Is4() {
		b := a.As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		v += n
		b[0] = byte(v >> 24)
		b[1] = byte(v >> 16)
		b[2] = byte(v >> 8)
		b[3] = byte(v)
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	carry := uint64(n)
	for i := 15; i >= 0 && carry > 0; i-- {
		sum := uint64(b[i]) + carry
		b[i] = byte(sum & 0xff)
		carry = sum >> 8
	}
	return netip.AddrFrom16(b)
}

// addrSub subtracts n from the address (no underflow protection;
// callers should not subtract past the network address).
func addrSub(a netip.Addr, n uint32) netip.Addr {
	if a.Is4() {
		b := a.As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		v -= n
		b[0] = byte(v >> 24)
		b[1] = byte(v >> 16)
		b[2] = byte(v >> 8)
		b[3] = byte(v)
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	borrow := uint64(n)
	for i := 15; i >= 0 && borrow > 0; i-- {
		v := uint64(b[i]) - (borrow & 0xff)
		nextBorrow := borrow >> 8
		if v > 0xff {
			// underflowed this byte; bring it back into range
			// and propagate one more borrow upward.
			b[i] = byte(v + 0x100)
			nextBorrow++
		} else {
			b[i] = byte(v)
		}
		borrow = nextBorrow
	}
	return netip.AddrFrom16(b)
}

// lastInPrefix returns the highest address that still falls inside p.
// For IPv4 this is the broadcast address; for IPv6 it's the all-host-
// bits-set address (the concept of "broadcast" doesn't exist in v6
// but the math is identical).
func lastInPrefix(p netip.Prefix) netip.Addr {
	addr := p.Masked().Addr()
	if addr.Is4() {
		b := addr.As4()
		hostBits := 32 - p.Bits()
		mask := uint32(0)
		if hostBits > 0 {
			mask = (uint32(1) << hostBits) - 1
		}
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | mask
		b[0] = byte(v >> 24)
		b[1] = byte(v >> 16)
		b[2] = byte(v >> 8)
		b[3] = byte(v)
		return netip.AddrFrom4(b)
	}
	b := addr.As16()
	hostBits := 128 - p.Bits()
	for i := 15; i >= 0 && hostBits > 0; i-- {
		if hostBits >= 8 {
			b[i] = 0xff
			hostBits -= 8
		} else {
			b[i] |= byte(0xff) >> (8 - hostBits)
			hostBits = 0
		}
	}
	return netip.AddrFrom16(b)
}

func normalizeHost(h string) string {
	h = strings.TrimSuffix(h, ".")
	h = strings.ToLower(h)
	return h
}
