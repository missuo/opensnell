/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"context"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// QUIC proxy mode (snell v5 feature)
// ============================================================================
//
// On the same port that snell-server listens for TCP, it ALSO listens for
// UDP. The first 1–2 packets from any new (src_ip, src_port) are
// expected to be a snell-encrypted envelope wrapping a QUIC Initial
// packet — this is how the client safely conveys the target host
// (without leaking SNI on the wire). Once the (src_ip:src_port) → target
// mapping is established, every subsequent UDP packet in either
// direction is forwarded as raw QUIC, with no additional snell framing.
//
// Wire format of the envelope (client → server, first packet only):
//
//   [salt        : 16 bytes random per packet]
//   [hdr_cipher  : 23 bytes = AEAD-Seal(K, nonce_0, hdr_plain)
//                  hdr_plain (7 bytes) = [
//                      0x04,             // frame type, same as the v4 TCP frame
//                      0x00, 0x00,
//                      padLen   (2B BE),
//                      payloadLen (2B BE),
//                  ]]
//   [padding     : padLen bytes (optional traffic-shape padding)]
//   [pay_cipher  : payloadLen + 16 = AEAD-Seal(K, nonce_1, payload)
//                  payload =  [
//                      0x01,             // snell protocol version
//                      0x01,             // command (QUIC tunnel)
//                      0x00,             // client ID length (snell convention)
//                      hostlen (1B),
//                      hostname,
//                      port (2B BE),
//                      inner_QUIC_packet,
//                  ]]
//
// Key derivation: K = Argon2id(psk_utf8, salt, t=3, m=8 KiB, p=1, 32)[:16]
//                 (same KDF as the v4 TCP frame)
// AEAD:           AES-128-GCM, 12-byte nonce (LE-encoded counter; 0 for
//                 header, 1 for payload)
//
// Discovery / interop notes: this format was reverse-engineered against
// the official Surge snell-server v5.0.1 (Nov 2025) by capturing real
// HTTP/3 traffic from a Surge client. See the project README for the
// methodology.

const (
	quicSaltLen      = 16
	quicHeaderPlain  = 7
	quicHeaderCipher = quicHeaderPlain + 16 // AEAD tag

	// QUIC mode reuses the v4 frame's first-byte type marker.
	quicFrameType byte = 4

	// snell request header inside the encrypted payload — these are not
	// the same as the QUIC payload's bytes; they describe which target
	// host the server should forward to.
	quicReqVersion byte = 1
	quicReqCommand byte = 1

	// Default idle timeout for a QUIC flow. After this much silence in
	// either direction, the flow is reaped and the upstream socket
	// closed. QUIC clients can do connection migration with idle gaps,
	// but for a proxy 5 minutes is more than enough headroom.
	quicFlowIdle = 5 * time.Minute

	// Per-direction read buffer. QUIC packets are MTU-sized; 64 KiB is
	// plenty and matches what the official server uses (observed via
	// strace: recvmmsg with iov_len=65536).
	quicMaxPacket = 65536
)

// quicFlow tracks one client→upstream UDP relay session.
type quicFlow struct {
	clientAddr   *net.UDPAddr
	upstream     net.Conn // udp connection to upstream, dialed via the server's egress-aware dialer
	lastActivity int64    // unix nano
	closeOnce    sync.Once
	stop         chan struct{}
}

func (f *quicFlow) touch() {
	// Loose atomicity: int64 writes are atomic on every supported
	// platform here (amd64/arm64). No need for atomic.Store; readers
	// only use the value to compare against time.Now().UnixNano() for
	// the idle reaper, so a stale read just delays reaping by one tick.
	f.lastActivity = time.Now().UnixNano()
}

func (f *quicFlow) close() {
	f.closeOnce.Do(func() {
		close(f.stop)
		_ = f.upstream.Close()
	})
}

// ServeQUIC listens on the same address as the TCP listener (but UDP)
// and implements snell v5 QUIC proxy mode. Returns when ctx is cancelled
// or the listener fails irrecoverably.
func (s *Server) ServeQUIC(ctx context.Context) error {
	lc := s.listenConfig()
	pc, err := lc.ListenPacket(ctx, "udp", s.cfg.Listen)
	if err != nil {
		return err
	}
	defer pc.Close()

	udpPC, ok := pc.(*net.UDPConn)
	if !ok {
		return errors.New("snell quic: listener is not *net.UDPConn")
	}

	s.logger.Info("snell quic listening", "addr", udpPC.LocalAddr().String())

	flows := newFlowTable()
	go flows.reaper(ctx, quicFlowIdle)

	go func() {
		<-ctx.Done()
		_ = udpPC.Close()
	}()

	buf := make([]byte, quicMaxPacket)
	for {
		n, raddr, err := udpPC.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logger.Debug("quic recv error", "err", err)
			continue
		}

		flow := flows.get(raddr)
		if flow != nil {
			// Existing flow — forward verbatim. Most QUIC packets fall here.
			if _, werr := flow.upstream.Write(buf[:n]); werr != nil {
				s.logger.Debug("quic forward to upstream failed", "err", werr)
				flows.remove(raddr)
				continue
			}
			flow.touch()
			continue
		}

		// New flow — try to decode the envelope.
		srcIP := ""
		if raddr != nil {
			srcIP = raddr.IP.String()
		}
		target, inner, user, derr := s.decodeQUICEnvelope(buf[:n], srcIP)
		if derr != nil {
			s.logger.Debug("quic envelope decode failed", "remote", raddr.String(), "err", derr)
			continue
		}

		upstream, derr := s.dialUDPUpstream(ctx, target)
		if derr != nil {
			s.logger.Warn("quic upstream dial failed", "target", target, "err", derr)
			continue
		}

		flow = &quicFlow{
			clientAddr: cloneUDPAddr(raddr),
			upstream:   upstream,
			stop:       make(chan struct{}),
		}
		flow.touch()
		flows.put(raddr, flow)
		s.logger.Info("snell quic flow",
			"client", raddr.String(),
			"target", target,
			"inner_size", len(inner),
			"user", user,
		)

		// Send the unwrapped QUIC initial upstream.
		if _, werr := flow.upstream.Write(inner); werr != nil {
			s.logger.Debug("quic initial forward failed", "target", target, "err", werr)
			flows.remove(raddr)
			continue
		}

		// Reverse-direction goroutine: read from upstream and forward
		// raw bytes back to the client on the listener socket.
		go s.quicReverseRelay(flow, udpPC, flows)
	}
}

func (s *Server) quicReverseRelay(flow *quicFlow, listener *net.UDPConn, flows *flowTable) {
	defer flows.remove(flow.clientAddr)
	buf := make([]byte, quicMaxPacket)
	for {
		_ = flow.upstream.SetReadDeadline(time.Now().Add(quicFlowIdle))
		n, err := flow.upstream.Read(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
				s.logger.Debug("quic upstream read error", "err", err)
			}
			return
		}
		if _, werr := listener.WriteToUDP(buf[:n], flow.clientAddr); werr != nil {
			if !errors.Is(werr, net.ErrClosed) {
				s.logger.Debug("quic client write error", "err", werr)
			}
			return
		}
		flow.touch()
		select {
		case <-flow.stop:
			return
		default:
		}
	}
}

func (s *Server) dialUDPUpstream(ctx context.Context, target string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	if s.cfg.EgressInterface != "" {
		dialer.Control = bindEgressInterface(s.cfg.EgressInterface)
	}
	if s.resolver != nil {
		dialer.Resolver = s.resolver
	}
	return dialer.DialContext(ctx, s.outboundNetwork("udp"), target)
}

// decodeQUICEnvelope attempts to interpret packet as a snell v5 QUIC
// envelope and return the embedded target ("host:port"), the inner
// raw QUIC packet bytes ready to forward upstream, and (in multi-user
// mode) the matched user's Name.
//
// Single-user mode (s.auth == nil) preserves the original fast path
// byte-for-byte: one Argon2 + one AES-GCM Open under cfg.PSK. Multi-
// user mode looks up the client's source IP in the same LRU that the
// TCP path uses, then falls back to a full scan on miss.
//
// srcIP is the QUIC client's source IP used as the LRU key; pass "" to
// skip LRU lookup (cold-path scan only).
func (s *Server) decodeQUICEnvelope(packet []byte, srcIP string) (target string, inner []byte, user string, err error) {
	if len(packet) < quicSaltLen+quicHeaderCipher+16 {
		return "", nil, "", errors.New("envelope too short")
	}
	salt := packet[:quicSaltLen]
	rest := packet[quicSaltLen:]
	if len(rest) < quicHeaderCipher {
		return "", nil, "", errors.New("envelope header truncated")
	}

	var (
		aead     cipher.AEAD
		matchedUser string
		matchedPSK  []byte
	)

	if s.auth == nil {
		// FAST PATH: single-PSK, identical to pre-multi-user code.
		aead, err = v4AEAD(s.psk, salt)
		if err != nil {
			return "", nil, "", err
		}
		matchedPSK = s.psk
	} else {
		// MULTI-USER PATH: trial-decrypt the 23-byte header against
		// each registered user (LRU-cached by srcIP).
		users := s.auth.store.ListUsers()
		if len(users) == 0 {
			return "", nil, "", errors.New("no authorized users")
		}
		headerCipher := rest[:quicHeaderCipher]

		// Hot path
		if srcIP != "" {
			if idx, ok := s.auth.lru.Get(srcIP); ok && idx >= 0 && idx < len(users) {
				if ae, ok := tryDecryptHeader(users[idx].PSK, salt, headerCipher); ok {
					aead = ae
					matchedUser = users[idx].Name
					matchedPSK = users[idx].PSK
				}
			}
		}
		// Cold path
		if aead == nil {
			matchIdx := s.auth.scanUsers(users, salt, headerCipher)
			if matchIdx < 0 {
				return "", nil, "", errors.New("no matching PSK")
			}
			ae, aerr := v4AEAD(users[matchIdx].PSK, salt)
			if aerr != nil {
				return "", nil, "", aerr
			}
			aead = ae
			matchedUser = users[matchIdx].Name
			matchedPSK = users[matchIdx].PSK
			if srcIP != "" {
				s.auth.lru.Put(srcIP, matchIdx)
			}
		}
	}
	_ = matchedPSK // we currently only need read-side AEAD for envelope decode
	nonceLen := aead.NonceSize()

	// AEAD.Open of the header was already done by tryDecryptHeader in
	// multi-user mode, but it returned only success/failure — we need
	// the plaintext here to extract padLen/payloadLen. Re-Open with
	// nonce 0 (AEAD is stateless; safe to Open twice).
	nonce0 := make([]byte, nonceLen)
	hdr, err := aead.Open(nil, nonce0, rest[:quicHeaderCipher], nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("header decrypt: %w", err)
	}
	if hdr[0] != quicFrameType {
		return "", nil, "", fmt.Errorf("unexpected frame type 0x%02x", hdr[0])
	}
	padLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	payloadLen := int(binary.BigEndian.Uint16(hdr[5:7]))

	payOff := quicHeaderCipher + padLen
	if payOff+payloadLen+aead.Overhead() > len(rest) {
		return "", nil, "", errors.New("envelope payload truncated")
	}
	// Decrypt payload with nonce 1 (LE-encoded counter, matching the v4
	// frame's per-frame nonce sequence).
	nonce1 := make([]byte, nonceLen)
	binary.LittleEndian.PutUint64(nonce1, 1)
	pay, err := aead.Open(nil, nonce1, rest[payOff:payOff+payloadLen+aead.Overhead()], nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("payload decrypt: %w", err)
	}
	target, inner, err = parseQUICRequest(pay)
	return target, inner, matchedUser, err
}

// parseQUICRequest extracts the target host/port and inner QUIC bytes
// from the decrypted envelope payload.
func parseQUICRequest(p []byte) (target string, inner []byte, err error) {
	if len(p) < 5 {
		return "", nil, errors.New("payload too short for request header")
	}
	if p[0] != quicReqVersion {
		return "", nil, fmt.Errorf("unsupported snell protocol version 0x%02x", p[0])
	}
	// p[1] is the command byte. The official server appears to send 0x01;
	// we don't enforce a specific value beyond "non-zero" to remain
	// forward-compatible.
	if p[1] == 0 {
		return "", nil, errors.New("invalid command 0")
	}
	clientIDLen := int(p[2])
	off := 3 + clientIDLen
	if off+1 > len(p) {
		return "", nil, errors.New("client id overruns payload")
	}
	hostLen := int(p[off])
	off++
	if off+hostLen+2 > len(p) {
		return "", nil, errors.New("hostname overruns payload")
	}
	host := string(p[off : off+hostLen])
	port := binary.BigEndian.Uint16(p[off+hostLen : off+hostLen+2])
	inner = p[off+hostLen+2:]
	if len(inner) == 0 {
		return "", nil, errors.New("inner QUIC packet missing")
	}
	target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return target, inner, nil
}

// flowTable is a concurrent-safe (client-addr → quicFlow) map.
type flowTable struct {
	mu sync.RWMutex
	m  map[string]*quicFlow
}

func newFlowTable() *flowTable {
	return &flowTable{m: make(map[string]*quicFlow)}
}

func (t *flowTable) get(addr *net.UDPAddr) *quicFlow {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[addr.String()]
}

func (t *flowTable) put(addr *net.UDPAddr, f *quicFlow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.m[addr.String()]; ok {
		old.close()
	}
	t.m[addr.String()] = f
}

func (t *flowTable) remove(addr *net.UDPAddr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f, ok := t.m[addr.String()]; ok {
		delete(t.m, addr.String())
		f.close()
	}
}

func (t *flowTable) reaper(ctx context.Context, idle time.Duration) {
	tick := time.NewTicker(idle / 4)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cutoff := time.Now().Add(-idle).UnixNano()
		var dead []string
		t.mu.RLock()
		for k, f := range t.m {
			if f.lastActivity < cutoff {
				dead = append(dead, k)
			}
		}
		t.mu.RUnlock()
		if len(dead) == 0 {
			continue
		}
		t.mu.Lock()
		for _, k := range dead {
			if f, ok := t.m[k]; ok {
				delete(t.m, k)
				f.close()
			}
		}
		t.mu.Unlock()
	}
}

func cloneUDPAddr(a *net.UDPAddr) *net.UDPAddr {
	cp := *a
	ip := make(net.IP, len(a.IP))
	copy(ip, a.IP)
	cp.IP = ip
	return &cp
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// EncodeQUICEnvelope is the client-side counterpart: given a target
// host:port and a raw QUIC initial packet, it produces the snell-encrypted
// envelope ready to send over UDP to the server.
//
// Provided for completeness and testing; the public opensnell client
// does not currently expose a QUIC proxy entry point (clients want raw
// QUIC sockets, which is typically done at the OS level by an
// enhanced-mode tunnel like Surge).
func EncodeQUICEnvelope(psk []byte, host string, port uint16, innerQUIC []byte) ([]byte, error) {
	if len(host) > 255 {
		return nil, errors.New("hostname too long")
	}
	if len(innerQUIC) == 0 {
		return nil, errors.New("inner QUIC packet missing")
	}

	// Build payload: [version][cmd][clientIDLen=0][hostlen][host][port][inner]
	payload := make([]byte, 0, 5+len(host)+len(innerQUIC))
	payload = append(payload, quicReqVersion, quicReqCommand, 0, byte(len(host)))
	payload = append(payload, host...)
	payload = binary.BigEndian.AppendUint16(payload, port)
	payload = append(payload, innerQUIC...)

	if len(payload) > 0xFFFF {
		return nil, errors.New("envelope payload too large")
	}

	salt := make([]byte, quicSaltLen)
	if _, err := io.ReadFull(cryptorand.Reader, salt); err != nil {
		return nil, err
	}
	aead, err := v4AEAD(psk, salt)
	if err != nil {
		return nil, err
	}

	header := make([]byte, quicHeaderPlain)
	header[0] = quicFrameType
	// padLen = 0 (we don't add random padding here; the official server
	// accepts padLen=0 fine, as confirmed in our captured traffic).
	binary.BigEndian.PutUint16(header[5:7], uint16(len(payload)))

	nonce0 := make([]byte, aead.NonceSize())
	hdrCipher := aead.Seal(nil, nonce0, header, nil)

	nonce1 := make([]byte, aead.NonceSize())
	binary.LittleEndian.PutUint64(nonce1, 1)
	payCipher := aead.Seal(nil, nonce1, payload, nil)

	out := make([]byte, 0, quicSaltLen+len(hdrCipher)+len(payCipher))
	out = append(out, salt...)
	out = append(out, hdrCipher...)
	out = append(out, payCipher...)
	return out, nil
}
