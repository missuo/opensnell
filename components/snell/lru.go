/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import "sync"

// ipLRU caches "this client IP → last user-index that authenticated
// successfully for them". It is the multi-user authenticator's hot path:
// a hit lets us skip the O(N) trial-decrypt scan and check only the one
// PSK the client is most likely to be holding.
//
// This is not strictly an LRU — under cache pressure it evicts an
// arbitrary entry rather than the least-recently-used one. The simpler
// random-eviction strategy is fine here because the working set (active
// client IPs) tends to be much smaller than the cache capacity in
// practice, so eviction is rare; and when it does happen, a wrong
// eviction just costs one extra cold-path scan, never an auth failure.
type ipLRU struct {
	max int

	mu    sync.Mutex
	store map[string]int
}

func newIPLRU(max int) *ipLRU {
	if max <= 0 {
		max = 1024
	}
	return &ipLRU{max: max, store: make(map[string]int, max)}
}

func (c *ipLRU) Get(k string) (int, bool) {
	c.mu.Lock()
	v, ok := c.store[k]
	c.mu.Unlock()
	return v, ok
}

func (c *ipLRU) Put(k string, v int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.store[k]; !exists && len(c.store) >= c.max {
		for evict := range c.store {
			delete(c.store, evict)
			break
		}
	}
	c.store[k] = v
}

// Len returns the current entry count. Test-only helper.
func (c *ipLRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.store)
}
