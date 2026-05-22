/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
)

// FakePool maps hostnames to synthetic IPv4 addresses within a fixed
// CIDR (e.g. 198.18.128.0/17). Each hostname is stable: the second
// allocation for the same hostname returns the same address. When the
// pool is exhausted, the least-recently-used hostname is evicted and
// its IP is reused.
//
// Concurrency: the pool is safe for use by many goroutines.
//
// Why bidirectional: at allocation time the DNS server hands out an
// IP; at TCP-connect time the TUN handler needs to recover the
// original hostname so it can talk to the snell server with
// AtypDomainName. Both directions are O(1).
type FakePool struct {
	mu sync.Mutex

	prefix    netip.Prefix
	firstUsed netip.Addr // smallest IP we ever hand out
	lastUsed  netip.Addr // largest IP we ever hand out

	byHost map[string]*list.Element
	byIP   map[netip.Addr]*list.Element
	lru    *list.List // *fakeEntry, front = MRU, back = LRU
	max    int        // max entries; 0 means unlimited (until prefix exhausted)
	next   netip.Addr // next IP to try when growing the pool
}

type fakeEntry struct {
	host string
	ip   netip.Addr
}

// NewFakePool constructs a pool over prefix (must be IPv4). The first
// few addresses are reserved (network address, "gateway", and a small
// headroom for the DNS server on the gateway), and the broadcast
// address is reserved at the top. Reserved addresses are never handed
// out as fake-IP for hostnames.
//
// reservedTop = TUN's own address + 1 broadcast. reservedHead = network
// address + a small headroom for the DNS server.
//
// max <= 0 means "as big as the prefix allows".
func NewFakePool(prefix netip.Prefix, max int) (*FakePool, error) {
	if !prefix.IsValid() || !prefix.Addr().Is4() {
		return nil, fmt.Errorf("fake-ip: prefix %q must be IPv4", prefix)
	}
	if prefix.Bits() > 30 {
		return nil, fmt.Errorf("fake-ip: prefix %q too small", prefix)
	}

	// First usable host = network + 16 (leaves room for .0 network,
	// .1 TUN gateway / DNS server, and a small reserved cushion).
	// Last usable = broadcast - 1.
	network := prefix.Masked().Addr()
	first := addrAdd(network, 16)
	bcast := broadcast(prefix)
	last := addrSub(bcast, 1)
	if !addrLE(first, last) {
		return nil, fmt.Errorf("fake-ip: prefix %q too small after reserving headroom", prefix)
	}

	capacity := int(addrDistance(first, last)) + 1
	if max <= 0 || max > capacity {
		max = capacity
	}

	return &FakePool{
		prefix:    prefix,
		firstUsed: first,
		lastUsed:  last,
		byHost:    make(map[string]*list.Element, max/4),
		byIP:      make(map[netip.Addr]*list.Element, max/4),
		lru:       list.New(),
		max:       max,
		next:      first,
	}, nil
}

// Prefix returns the CIDR the pool draws from.
func (p *FakePool) Prefix() netip.Prefix { return p.prefix }

// Gateway returns the IP the TUN device + DNS server occupy. This is
// the first usable address in the prefix (e.g. 198.18.128.1 for
// 198.18.128.0/17), which is excluded from fake-IP allocations.
func (p *FakePool) Gateway() netip.Addr {
	return addrAdd(p.prefix.Masked().Addr(), 1)
}

// Allocate returns a fake IPv4 for the given hostname. Repeated calls
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
		// pool has room — hand out the next sequential IP.
		ip = p.next
		p.next = addrAdd(p.next, 1)
	} else {
		// pool full — evict the LRU entry, reuse its slot.
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

// Lookup returns the hostname previously allocated to ip, marking the
// entry as recently used. ok=false means the IP is in our prefix range
// but has no mapping (e.g. an attacker / scanner hit a random fake-IP)
// — callers should drop such connections.
func (p *FakePool) Lookup(ip netip.Addr) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	el, ok := p.byIP[ip]
	if !ok {
		return "", false
	}
	p.lru.MoveToFront(el)
	return el.Value.(*fakeEntry).host, true
}

// Contains reports whether ip falls inside the pool's prefix.
func (p *FakePool) Contains(ip netip.Addr) bool {
	return p.prefix.Contains(ip)
}

// Len returns the number of currently-mapped hostnames.
func (p *FakePool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byHost)
}

// ----- IPv4 arithmetic helpers (netip.Addr doesn't expose +/- directly) -----

func addrAdd(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:])
	v += n
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func addrSub(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:])
	v -= n
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func addrLE(a, b netip.Addr) bool {
	x := a.As4()
	y := b.As4()
	return binary.BigEndian.Uint32(x[:]) <= binary.BigEndian.Uint32(y[:])
}

func addrDistance(a, b netip.Addr) uint32 {
	x := a.As4()
	y := b.As4()
	return binary.BigEndian.Uint32(y[:]) - binary.BigEndian.Uint32(x[:])
}

func broadcast(p netip.Prefix) netip.Addr {
	masked := p.Masked().Addr().As4()
	mask := ^(uint32(0xFFFFFFFF) << (32 - p.Bits()))
	v := binary.BigEndian.Uint32(masked[:]) | mask
	binary.BigEndian.PutUint32(masked[:], v)
	return netip.AddrFrom4(masked)
}

func normalizeHost(h string) string {
	h = strings.TrimSuffix(h, ".")
	h = strings.ToLower(h)
	return h
}
