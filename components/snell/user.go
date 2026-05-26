/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"sync/atomic"
)

// User identifies one authorized client and the PSK it must hold.
//
// Snell's wire protocol carries no user identifier; the server only knows
// "this 16-byte salt + this 23-byte AEAD'd header decrypted cleanly under
// PSK X, therefore the client must be the user that holds X". The Name
// field is the server's own label for the matched user (e.g., the panel's
// UUID), used for traffic accounting and limit lookups — it never travels
// on the wire.
type User struct {
	Name string
	PSK  []byte
}

// UserStore is a dynamic view onto the set of currently-authorized users.
// Implementations MUST be safe for concurrent use; ListUsers is called on
// every new connection's authentication path, so the implementation should
// back it with an atomic snapshot rather than locking around the whole
// authentication critical section.
//
// The returned slice MUST NOT be mutated by the caller. Implementations
// are free to return the same backing slice across calls (the auth path
// never modifies it).
type UserStore interface {
	ListUsers() []User
}

// StaticUserStore is a trivial UserStore backed by an atomic snapshot. It
// is suitable for both standalone servers (set users once) and panel-driven
// servers (call SetUsers on every panel pull). All readers see a consistent
// snapshot without locking the auth fast path.
type StaticUserStore struct {
	users atomic.Pointer[[]User]
}

// NewStaticUserStore constructs a StaticUserStore pre-loaded with the given
// users. The slice is copied; the caller is free to mutate the original.
func NewStaticUserStore(users []User) *StaticUserStore {
	s := &StaticUserStore{}
	s.SetUsers(users)
	return s
}

// SetUsers atomically replaces the store's user set. Safe to call from
// background goroutines (e.g., the panel-pull loop) while authentication
// is happening on the data path; the old snapshot stays valid for any
// auth attempt that already loaded it.
func (s *StaticUserStore) SetUsers(users []User) {
	cp := make([]User, len(users))
	copy(cp, users)
	s.users.Store(&cp)
}

// ListUsers returns the current snapshot. Returns an empty (non-nil) slice
// when SetUsers has never been called.
func (s *StaticUserStore) ListUsers() []User {
	if p := s.users.Load(); p != nil {
		return *p
	}
	return nil
}

// Len returns the current user count without copying the slice. Useful
// for metrics and config validation.
func (s *StaticUserStore) Len() int {
	if p := s.users.Load(); p != nil {
		return len(*p)
	}
	return 0
}

// UserContext is implemented by snell connections accepted under multi-user
// mode. Callers may type-assert an accepted net.Conn to UserContext to
// retrieve which user's PSK authenticated the connection — this is the
// canonical place to look up panel-side traffic counters and rate limits.
//
// Connections accepted under single-user mode (ServerConfig.PSK set,
// UserStore nil) do not implement this interface.
type UserContext interface {
	UserName() string
}
