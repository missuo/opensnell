/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import "testing"

func TestNormalizeListenAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Bracketless IPv6 (official snell-server form) gets bracketed.
		{"::0:59283", "[::0]:59283"},
		{":::2333", "[::]:2333"},
		{"2001:db8::1:8080", "[2001:db8::1]:8080"},
		// Already-valid forms are untouched.
		{"0.0.0.0:2333", "0.0.0.0:2333"},
		{"[::]:2333", "[::]:2333"},
		{"[::0]:59283", "[::0]:59283"},
		{":2333", ":2333"},
		{"127.0.0.1:1080", "127.0.0.1:1080"},
		// Things we can't confidently interpret pass through unchanged.
		{"", ""},
		{"2333", "2333"},
		{"::1", "::1"},
	}
	for _, c := range cases {
		if got := normalizeListenAddr(c.in); got != c.want {
			t.Errorf("normalizeListenAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
