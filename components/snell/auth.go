/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"crypto/cipher"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"time"
)

// authResult is what multiUserAuth.authenticate returns on success: the
// matched user, an AEAD already keyed for that user's (psk, salt), and
// the raw 23-byte AEAD'd header that auth consumed during the trial
// decrypt — the v4Reader needs that header re-delivered so it can step
// its nonce counter through the same Open call.
type authResult struct {
	user           User
	readAEAD       cipher.AEAD
	prefetchedSalt []byte // 16 bytes — already consumed from the wire
	prefetchedHdr  []byte // 23 bytes — already consumed from the wire
}

// multiUserAuth is the per-server authenticator that owns the user store
// and the IP→user-index LRU. One instance per Server.
type multiUserAuth struct {
	store UserStore

	lru *ipLRU

	// unauthDelay is the constant-time sleep added before returning an
	// auth failure. Without it, a client doing a binary search over
	// PSKs could observe whether the cold-path full-table scan ran
	// (slow) vs the LRU hot path (fast) and infer "is my guess in
	// someone else's LRU slot". Defaults to 50 ms.
	unauthDelay time.Duration

	// parallelThreshold: when the user table exceeds this length the
	// cold-path scan fans out across GOMAXPROCS workers. Below the
	// threshold the serial scan is cheaper than the goroutine spin-up.
	// Defaults to 16.
	parallelThreshold int
}

// newMultiUserAuth constructs a default-configured authenticator. The
// LRU capacity is sized for a node with a few thousand active clients;
// V2Node-style panels typically see well under that.
func newMultiUserAuth(store UserStore) *multiUserAuth {
	return &multiUserAuth{
		store:             store,
		lru:               newIPLRU(4096),
		unauthDelay:       50 * time.Millisecond,
		parallelThreshold: 16,
	}
}

// authenticate reads salt + first AEAD header from r, identifies which
// user holds the matching PSK, and returns the materials the v4Conn needs
// to keep going (an AEAD already constructed, plus the bytes we consumed
// so v4Reader can replay them).
//
// remoteIP is used purely as an LRU key; pass "" to disable LRU lookup
// for this connection.
//
// Behavior on failure:
//   - Returns the original io.EOF/network error verbatim when the read
//     itself fails (so the caller can distinguish disconnect from auth
//     failure).
//   - Returns an "auth failed" error after a constant-time sleep when
//     the 23-byte header didn't decrypt under any registered PSK.
func (a *multiUserAuth) authenticate(r io.Reader, remoteIP string) (*authResult, error) {
	salt := make([]byte, v4SaltSize)
	if _, err := io.ReadFull(r, salt); err != nil {
		return nil, err
	}
	headerCipher := make([]byte, v4HeaderCipherSize)
	if _, err := io.ReadFull(r, headerCipher); err != nil {
		return nil, err
	}

	users := a.store.ListUsers()
	if len(users) == 0 {
		time.Sleep(a.unauthDelay)
		return nil, errors.New("snell: no authorized users")
	}

	// Hot path: try the LRU-cached user index first. A correct cache
	// hit makes a fresh connection from an established client cost
	// exactly one Argon2 + one AES-GCM Open — the same as the
	// single-user fast path elsewhere in the server.
	if remoteIP != "" {
		if idx, ok := a.lru.Get(remoteIP); ok && idx >= 0 && idx < len(users) {
			if aead, ok := tryDecryptHeader(users[idx].PSK, salt, headerCipher); ok {
				return &authResult{
					user:           users[idx],
					readAEAD:       aead,
					prefetchedSalt: salt,
					prefetchedHdr:  headerCipher,
				}, nil
			}
			// LRU stale (user rotated PSK, panel reshuffled the list,
			// etc) — fall through to the cold-path scan.
		}
	}

	// Cold path: trial-decrypt against every registered user.
	matchIdx := a.scanUsers(users, salt, headerCipher)
	if matchIdx < 0 {
		time.Sleep(a.unauthDelay)
		return nil, errors.New("snell: authentication failed (no matching PSK)")
	}

	aead, err := v4AEAD(users[matchIdx].PSK, salt)
	if err != nil {
		return nil, err
	}
	if remoteIP != "" {
		a.lru.Put(remoteIP, matchIdx)
	}
	return &authResult{
		user:           users[matchIdx],
		readAEAD:       aead,
		prefetchedSalt: salt,
		prefetchedHdr:  headerCipher,
	}, nil
}

// scanUsers returns the index of the first user whose PSK decrypts the
// header cleanly, or -1. Serial below parallelThreshold; parallel above.
//
// Parallel mode is "first match wins" — once any worker reports success,
// the others stop. We do NOT enforce constant-time per-attempt cost
// across workers because:
//   1. AEAD.Open is variable-time on FAILURE (returns immediately after
//      tag mismatch) but constant-time on SUCCESS (always processes the
//      same ~23 bytes). The relevant timing leak — "how long did the
//      whole auth take" — is masked by the unauthDelay on failure.
//   2. Argon2id is the dominant cost (~10–50µs); AES-GCM tag check is
//      ~100ns. So the timing skew between users is dominated by Argon2,
//      whose runtime depends only on (psk, salt) length and the fixed
//      parameters — uniform across all users.
func (a *multiUserAuth) scanUsers(users []User, salt, hdr []byte) int {
	if len(users) <= a.parallelThreshold {
		for i := range users {
			if _, ok := tryDecryptHeader(users[i].PSK, salt, hdr); ok {
				return i
			}
		}
		return -1
	}
	return a.scanUsersParallel(users, salt, hdr)
}

func (a *multiUserAuth) scanUsersParallel(users []User, salt, hdr []byte) int {
	workers := runtime.GOMAXPROCS(0)
	if workers < 2 {
		workers = 2
	}
	if workers > len(users) {
		workers = len(users)
	}

	type job struct{ from, to int }
	jobs := make([]job, workers)
	chunk := (len(users) + workers - 1) / workers
	for i := 0; i < workers; i++ {
		from := i * chunk
		to := from + chunk
		if to > len(users) {
			to = len(users)
		}
		jobs[i] = job{from, to}
	}

	resultCh := make(chan int, workers)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(rng job) {
			defer wg.Done()
			for k := rng.from; k < rng.to; k++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := tryDecryptHeader(users[k].PSK, salt, hdr); ok {
					select {
					case resultCh <- k:
					default:
					}
					return
				}
			}
		}(jobs[i])
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	match := -1
	for r := range resultCh {
		match = r
		close(stop)
		// Drain remaining results so the workers can exit without
		// blocking on a closed receiver.
		for range resultCh {
		}
		break
	}
	return match
}

// tryDecryptHeader does one trial decryption of the 23-byte AEAD header
// under one user's PSK. Returns the ready-to-use AEAD on success.
//
// The acceptance criteria are:
//   - AEAD.Open succeeds (tag check passes; ~2^-128 false positive rate)
//   - The first plaintext byte is the v4 frame-type marker (0x04). This
//     adds a second filter to guard against the vanishingly-small chance
//     of an AEAD tag collision.
func tryDecryptHeader(psk, salt, headerCipher []byte) (cipher.AEAD, bool) {
	aead, err := v4AEAD(psk, salt)
	if err != nil {
		return nil, false
	}
	nonce := make([]byte, aead.NonceSize())
	plain, err := aead.Open(nil, nonce, headerCipher, nil)
	if err != nil {
		return nil, false
	}
	if len(plain) != v4HeaderPlainSize || plain[0] != 4 {
		return nil, false
	}
	return aead, true
}

// extractIP pulls the host portion out of a net.Addr, returning "" on
// anything we don't recognize. Used as the LRU key for IPv6-mapped-IPv4
// the IPv4 form is returned (strip the ::ffff: prefix) so a dual-stack
// client gets one cache slot regardless of which family it dialed from.
func extractIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return host
}
