/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package tun

import (
	"fmt"
	"net/netip"
	"testing"
)

func mustParsePrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

func TestFakePoolAllocateStable(t *testing.T) {
	pool, err := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}

	a, err := pool.Allocate("example.com")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if !pool.Contains(a) {
		t.Errorf("allocated %v not in prefix", a)
	}

	b, err := pool.Allocate("example.com")
	if err != nil {
		t.Fatalf("allocate again: %v", err)
	}
	if a != b {
		t.Errorf("expected stable mapping, got %v then %v", a, b)
	}

	host, ok := pool.Lookup(a)
	if !ok {
		t.Fatalf("Lookup(%v) miss", a)
	}
	if host != "example.com" {
		t.Errorf("expected example.com, got %q", host)
	}
}

func TestFakePoolDistinctHostsGetDistinctIPs(t *testing.T) {
	pool, err := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}

	a, _ := pool.Allocate("a.example")
	b, _ := pool.Allocate("b.example")
	if a == b {
		t.Errorf("expected distinct IPs, both got %v", a)
	}
}

func TestFakePoolHostnameNormalization(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)

	a, _ := pool.Allocate("Example.COM")
	b, _ := pool.Allocate("example.com.")
	c, _ := pool.Allocate("example.com")
	if a != b || b != c {
		t.Errorf("normalization broken: %v %v %v", a, b, c)
	}
}

func TestFakePoolLRUEviction(t *testing.T) {
	// Tiny pool over a small (but valid) prefix. Bits=29 → 8 addrs;
	// after reserving 16 head + 1 broadcast there's 0 usable, so we
	// use a /28 instead: 16 addrs, headroom 16 → 0 usable too. Fall
	// back to /27 (32 addrs) - 16 - 1 = 15 usable. Cap to max=3 so
	// the test can exercise eviction.
	pool, err := NewFakePool(mustParsePrefix(t, "198.18.128.0/27"), 3)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}

	ips := make(map[string]netip.Addr)
	for _, h := range []string{"a", "b", "c"} {
		ip, err := pool.Allocate(h)
		if err != nil {
			t.Fatalf("allocate %s: %v", h, err)
		}
		ips[h] = ip
	}
	if pool.Len() != 3 {
		t.Fatalf("expected len=3, got %d", pool.Len())
	}

	// Now allocate "d" → pool is full → LRU "a" gets evicted, its IP
	// reused for "d".
	ipD, err := pool.Allocate("d")
	if err != nil {
		t.Fatalf("allocate d: %v", err)
	}
	if ipD != ips["a"] {
		t.Errorf("expected d to reuse a's IP %v, got %v", ips["a"], ipD)
	}
	if _, ok := pool.Lookup(ips["a"]); !ok {
		t.Errorf("after eviction, lookup of reused IP should find d")
	}

	// Lookup "b" to mark it as recently used. Then allocate "e" → "c"
	// should be the next LRU victim.
	if _, ok := pool.Lookup(ips["b"]); !ok {
		t.Errorf("b should still be mapped")
	}
	ipE, _ := pool.Allocate("e")
	if ipE != ips["c"] {
		t.Errorf("expected e to reuse c's IP %v, got %v", ips["c"], ipE)
	}
}

func TestFakePoolGateway(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)
	want := netip.MustParseAddr("198.18.128.1")
	if pool.Gateway() != want {
		t.Errorf("gateway = %v, want %v", pool.Gateway(), want)
	}
}

func TestFakePoolEmptyHostnameRejected(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)
	_, err := pool.Allocate("")
	if err == nil {
		t.Errorf("expected error for empty hostname")
	}
	_, err = pool.Allocate(".")
	if err == nil {
		t.Errorf("expected error for hostname '.' (normalizes to empty)")
	}
}

func TestFakePoolContains(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "198.18.128.0/17"), 0)
	cases := []struct {
		ip   string
		want bool
	}{
		{"198.18.128.5", true},
		{"198.18.200.10", true},
		{"198.18.255.255", true},
		{"198.18.127.255", false},
		{"198.19.0.0", false},
		{"10.0.0.1", false},
	}
	for _, c := range cases {
		got := pool.Contains(netip.MustParseAddr(c.ip))
		if got != c.want {
			t.Errorf("Contains(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// IPv6 pool — same semantics as IPv4 but the prefix is v6.
func TestFakePoolIPv6Stable(t *testing.T) {
	pool, err := NewFakePool(mustParsePrefix(t, "fdee::/96"), 0)
	if err != nil {
		t.Fatalf("new v6 pool: %v", err)
	}
	a, err := pool.Allocate("example.com")
	if err != nil {
		t.Fatalf("v6 allocate: %v", err)
	}
	if !a.Is6() || a.Is4() {
		t.Errorf("expected v6 addr, got %v (Is6=%v Is4=%v)", a, a.Is6(), a.Is4())
	}
	if !pool.Contains(a) {
		t.Errorf("allocated v6 %v not in v6 prefix", a)
	}
	b, _ := pool.Allocate("example.com")
	if a != b {
		t.Errorf("v6 stable mapping broken: %v vs %v", a, b)
	}
	host, ok := pool.Lookup(a)
	if !ok || host != "example.com" {
		t.Errorf("v6 reverse lookup: ok=%v host=%q", ok, host)
	}
}

func TestFakePoolIPv6Gateway(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "fdee::/96"), 0)
	want := netip.MustParseAddr("fdee::1")
	if pool.Gateway() != want {
		t.Errorf("v6 gateway = %v, want %v", pool.Gateway(), want)
	}
}

func TestFakePoolIPv6SequentialAllocation(t *testing.T) {
	pool, _ := NewFakePool(mustParsePrefix(t, "fdee::/96"), 0)
	first, _ := pool.Allocate("a.example")
	second, _ := pool.Allocate("b.example")
	if first == second {
		t.Errorf("expected distinct v6 IPs, both got %v", first)
	}
	// Two sequential allocations should differ by exactly 1.
	if addrAdd(first, 1) != second {
		t.Errorf("v6 not sequential: %v then %v", first, second)
	}
}

func TestAddrArithmeticIPv4(t *testing.T) {
	cases := []struct {
		from, want string
		n          uint32
		op         string
	}{
		{"198.18.128.0", "198.18.128.16", 16, "add"},
		{"198.18.128.16", "198.18.128.0", 16, "sub"},
		{"0.0.0.255", "0.0.1.0", 1, "add"}, // carry across byte boundary
		{"0.0.1.0", "0.0.0.255", 1, "sub"},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.from)
		var got netip.Addr
		if c.op == "add" {
			got = addrAdd(a, c.n)
		} else {
			got = addrSub(a, c.n)
		}
		if got.String() != c.want {
			t.Errorf("%s(%s, %d) = %s, want %s", c.op, c.from, c.n, got, c.want)
		}
	}
}

func TestAddrArithmeticIPv6(t *testing.T) {
	cases := []struct {
		from, want string
		n          uint32
		op         string
	}{
		{"fdee::", "fdee::10", 16, "add"},
		{"fdee::10", "fdee::", 16, "sub"},
		{"fdee::ff", "fdee::100", 1, "add"},     // carry across byte
		{"fdee::100", "fdee::ff", 1, "sub"},
		{"fdee::ffff", "fdee::1:0", 1, "add"},   // carry into the next 16-bit group
		{"fdee::1:0", "fdee::ffff", 1, "sub"},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.from)
		var got netip.Addr
		if c.op == "add" {
			got = addrAdd(a, c.n)
		} else {
			got = addrSub(a, c.n)
		}
		want := netip.MustParseAddr(c.want)
		if got != want {
			t.Errorf("%s(%s, %d) = %s, want %s", c.op, c.from, c.n, got, want)
		}
	}
}

func TestLastInPrefix(t *testing.T) {
	cases := []struct {
		prefix, want string
	}{
		{"198.18.128.0/17", "198.18.255.255"},
		{"198.18.128.0/24", "198.18.128.255"},
		{"10.0.0.0/8", "10.255.255.255"},
		{"fdee::/96", "fdee::ffff:ffff"},
		{"fdee::/112", "fdee::ffff"},
		{"fd00::/8", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
	}
	for _, c := range cases {
		p := netip.MustParsePrefix(c.prefix)
		want := netip.MustParseAddr(c.want)
		got := lastInPrefix(p)
		if got != want {
			t.Errorf("lastInPrefix(%s) = %s, want %s", c.prefix, got, want)
		}
	}
}

// rough sanity check: allocating a large number of hosts must not
// outrun the prefix size + headroom.
func TestFakePoolDoesNotOverrunPrefix(t *testing.T) {
	pool, err := NewFakePool(mustParsePrefix(t, "198.18.128.0/24"), 0)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	// /24 = 256, minus 16 headroom minus 1 broadcast = 239 max.
	for i := 0; i < 500; i++ {
		ip, err := pool.Allocate(fmt.Sprintf("host%d.example", i))
		if err != nil {
			t.Fatalf("allocate host%d: %v", i, err)
		}
		if !pool.Contains(ip) {
			t.Fatalf("allocated %v outside prefix on iter %d", ip, i)
		}
	}
	if pool.Len() > 239 {
		t.Errorf("pool len %d exceeds prefix capacity 239", pool.Len())
	}
}
