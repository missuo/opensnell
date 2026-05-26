/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/missuo/opensnell/components/obfs"
	"github.com/missuo/opensnell/components/utils"
	"github.com/missuo/opensnell/components/utils/pool"
)

// ServerConfig is the runtime configuration for Server.
type ServerConfig struct {
	Listen string // host:port to bind on

	// PSK is the single-user shared secret. Set this for standalone
	// servers serving one client. Mutually exclusive with UserStore.
	// When both are set, UserStore wins and PSK is ignored.
	PSK string

	// UserStore enables multi-user mode: every new connection is
	// authenticated by trial-decrypting its first frame against each
	// registered user's PSK. An LRU cache keyed on the client's IP
	// keeps the steady-state cost at one Argon2 + one AES-GCM per
	// connection (same as the single-user fast path). See package
	// docs in user.go and auth.go for details.
	//
	// When nil the server runs in the original single-PSK fast path
	// with zero added latency and zero added allocations per
	// connection. The multi-user code paths are entirely opt-in.
	UserStore UserStore

	// OnAuthorize, when non-nil, is invoked after a TCP CONNECT request
	// has been authenticated and parsed. It receives the matched user's
	// name, the client's remote address, the target host:port, and a
	// dialer pre-configured with the server's egress / DNS / TFO knobs.
	//
	// Returning a non-nil net.Conn instructs the server to relay using
	// that conn — callers can wrap the dialer's output to count traffic
	// or apply per-user rate limits before returning.
	//
	// Returning (nil, error) rejects the request: the server sends an
	// error response frame to the client and closes. Use this to
	// enforce per-user quotas (over-limit, device-cap exceeded, etc).
	//
	// Leave nil to use the built-in dial-and-relay logic — that's the
	// only behavior in single-user mode.
	OnAuthorize func(ctx context.Context, info ConnContext, dialer ContextDialer) (net.Conn, error)

	ObfsMode string // "", "off", "http", "tls"
	UDP      bool   // accept UDP-over-TCP requests
	DialTimeout time.Duration

	// EgressInterface, when non-empty, forces all outbound sockets
	// (upstream TCP dials and UDP-over-TCP listeners) to be bound to the
	// named local interface. Useful when the server host has multiple
	// network paths and you want to pin egress to a specific one.
	//
	// Implemented via SO_BINDTODEVICE on Linux and IP_BOUND_IF on macOS.
	// Other platforms return an error if this field is set.
	EgressInterface string

	// QUIC enables snell v5's QUIC proxy mode on the same UDP port as
	// the TCP listener. When the server accepts a UDP packet whose
	// (src_ip, src_port) has no existing flow mapping, it tries to
	// decode it as an encrypted snell envelope wrapping a QUIC Initial
	// packet (which conveys the target host); once decoded, the
	// (src, dst) mapping is recorded and all subsequent UDP packets in
	// both directions are forwarded as raw QUIC.
	//
	// Defaults to true (matching the official Surge snell-server).
	// Disable only if you don't want the UDP port active at all.
	QUIC bool

	// IPv6 controls whether the server's outbound dials (upstream TCP
	// connects, UDP-over-TCP forwarders, QUIC proxy upstreams) are
	// allowed to use IPv6 addresses. When false, outbound is constrained
	// to "tcp4" / "udp4" — useful on hosts whose IPv6 path is broken or
	// you simply want to avoid AAAA lookups slowing down dial-time.
	//
	// Defaults to true (matching the official Surge snell-server's
	// `ipv6 = true`). Note this only affects outbound; what addresses
	// the server LISTENS on is controlled by `Listen`.
	IPv6 bool

	// TFO enables TCP Fast Open (RFC 7413) on the inbound TCP listener
	// AND on outbound upstream TCP dials. With TFO, the client's first
	// data write can ride along in the SYN, eliminating one round-trip
	// per fresh TCP connection. Surge's per-proxy `tfo=true` configures
	// the same.
	//
	// Linux only for now (uses TCP_FASTOPEN and TCP_FASTOPEN_CONNECT
	// setsockopt). On other platforms the option is silently no-op
	// (macOS may still negotiate TFO transparently via its kernel
	// sysctl, but we don't actively force it). Off by default; before
	// enabling on Linux make sure
	// `cat /proc/sys/net/ipv4/tcp_fastopen` is `3` (or at least has the
	// server bit set: 2).
	TFO bool

	// DNS, when non-empty, overrides the host's default resolver for
	// upstream hostname resolution. Each entry is an IP literal (v4 or
	// v6) with an optional `:port` suffix; if no port is given, 53 is
	// assumed. Servers are tried in order on each lookup until one
	// returns a response. When empty, the host's system resolver is
	// used (typically /etc/resolv.conf).
	//
	// Matches the official Surge snell-server `dns = …` directive
	// added in v4.1.0. Equivalent log line at startup is "effective
	// DNS: <addr>" for each configured server.
	DNS []string

	// Brutal enables apernet/tcp-brutal congestion control on every
	// accepted TCP connection from a snell client. EXPERIMENTAL:
	// requires the brutal kernel module loaded on Linux. The rate is
	// applied PER CONNECTION; snell has no native multiplexing, so this
	// is only safe when you trust there is at most one heavy concurrent
	// stream at a time. Off by default.
	Brutal bool

	// BrutalMbps is the per-connection downstream rate (Mbit/s) applied
	// when Brutal is enabled. Must be > 0 if Brutal is true.
	BrutalMbps int

	// BrutalCwndGain is the cwnd-gain in tenths (15 = 1.5x, 20 = 2.0x).
	// Optional, default 15 (matches the brutal example).
	BrutalCwndGain int
}

// Server is a snell v4/v5 server. Use NewServer + Serve, or pass an
// accepted net.Listener to ServeListener if you want to manage the socket
// yourself.
type Server struct {
	cfg      ServerConfig
	psk      []byte
	logger   *slog.Logger
	resolver *net.Resolver // nil when cfg.DNS is empty (use system resolver)

	// auth is non-nil iff cfg.UserStore is non-nil. When nil, the
	// server runs the single-PSK fast path with no behavior change
	// from before multi-user support was added.
	auth *multiUserAuth
}

// ConnContext describes one authenticated snell connection to the
// OnAuthorize callback. All fields are valid for a TCP CONNECT request
// (Command == CommandConnect or CommandConnectV2).
type ConnContext struct {
	// User is the matched user's Name (the panel-side UUID for V2Node).
	// Empty when the server is running in single-PSK fast path mode.
	User string

	// RemoteAddr is the client's address as seen by the listener,
	// post-obfs (the obfs layer is purely transport-level and does not
	// rewrite addresses).
	RemoteAddr net.Addr

	// Target is the upstream destination in "host:port" form, as
	// parsed from the snell CONNECT request.
	Target string

	// Command is one of CommandConnect / CommandConnectV2.
	Command byte

	// Reuse mirrors the v2 reuse-mode bit (CommandConnectV2).
	Reuse bool
}

// ContextDialer is the dialer interface passed to OnAuthorize. The
// concrete value returned by Server is pre-configured with the server's
// egress-interface, IPv6, TFO, and custom DNS settings — callers can use
// it as-is to inherit those, or substitute their own.
type ContextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// buildResolver returns a net.Resolver that round-trips its DNS queries
// through the user-configured `dns = …` servers, or nil when no custom
// DNS servers are configured (in which case the system resolver is used,
// matching the historical behavior).
//
// Each upstream is an "ip[:port]" string; missing port defaults to 53.
// Servers are tried in order; the first to return a response wins.
// PreferGo: true ensures we go through this Dial hook rather than
// falling back to libc's getaddrinfo (which would silently ignore our
// custom resolver list).
func (s *Server) buildResolver() *net.Resolver {
	if len(s.cfg.DNS) == 0 {
		return nil
	}
	servers := make([]string, 0, len(s.cfg.DNS))
	for _, entry := range s.cfg.DNS {
		host := strings.TrimSpace(entry)
		if host == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "53")
		}
		servers = append(servers, host)
	}
	if len(servers) == 0 {
		return nil
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			var lastErr error
			for _, server := range servers {
				conn, err := d.DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("no configured DNS server is reachable")
			}
			return nil, lastErr
		},
	}
}

func NewServer(cfg ServerConfig, logger *slog.Logger) (*Server, error) {
	if cfg.PSK == "" && cfg.UserStore == nil {
		return nil, errors.New("snell server requires psk or user store")
	}
	switch cfg.ObfsMode {
	case "", "off", "http", "tls":
	default:
		return nil, fmt.Errorf("snell server: unknown obfs mode %q", cfg.ObfsMode)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, psk: []byte(cfg.PSK), logger: logger}
	if cfg.UserStore != nil {
		s.auth = newMultiUserAuth(cfg.UserStore)
		logger.Info("snell multi-user mode enabled (single-PSK fast path bypassed)")
	}
	if cfg.EgressInterface != "" {
		logger.Info("egress interface", "name", cfg.EgressInterface)
	}
	if cfg.QUIC {
		logger.Info("snell quic proxy mode enabled")
	}
	if r := s.buildResolver(); r != nil {
		s.resolver = r
		for _, addr := range cfg.DNS {
			logger.Info("effective DNS", "server", addr)
		}
	}
	if cfg.TFO {
		if !tfoSupported() {
			logger.Warn("tfo requested but not supported on this platform; relying on kernel-default TFO behavior if any")
		} else {
			if ok, why := tfoListenerReady(); !ok {
				logger.Warn("tfo enabled in config but kernel listener support disabled", "reason", why)
			} else {
				logger.Info("tcp fast open enabled (listener)")
			}
			if ok, why := tfoDialerReady(); !ok {
				logger.Warn("tfo enabled in config but kernel client support disabled", "reason", why)
			} else {
				logger.Info("tcp fast open enabled (upstream dialer)")
			}
		}
	}
	if cfg.Brutal {
		if cfg.BrutalMbps <= 0 {
			return nil, errors.New("brutal: brutal-mbps must be > 0 when brutal is enabled")
		}
		if cfg.BrutalCwndGain <= 0 {
			cfg.BrutalCwndGain = 15
		}
		if !brutalSupported() {
			logger.Warn("tcp-brutal requested but only supported on Linux; setting will be ignored",
				"mbps", cfg.BrutalMbps)
		} else {
			logger.Info("tcp-brutal enabled (EXPERIMENTAL)",
				"mbps", cfg.BrutalMbps, "cwnd_gain", cfg.BrutalCwndGain)
		}
		s.cfg = cfg
	}
	return s, nil
}

// chainListenControl combines multiple Control hooks into one. Each is
// run in order; the first non-nil error stops the chain.
func chainListenControl(fns ...func(string, string, syscall.RawConn) error) func(string, string, syscall.RawConn) error {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(network, addr string, c syscall.RawConn) error {
		for _, fn := range fns {
			if fn == nil {
				continue
			}
			if err := fn(network, addr, c); err != nil {
				return err
			}
		}
		return nil
	}
}

// maybeApplyBrutal applies tcp-brutal congestion control to conn when
// enabled in the server config. Logged-but-non-fatal on failure (the
// kernel module may not be installed; we prefer working-with-default-CC
// over refusing to serve the request).
func (s *Server) maybeApplyBrutal(conn net.Conn) {
	if !s.cfg.Brutal || !brutalSupported() {
		return
	}
	rate := uint64(s.cfg.BrutalMbps) * 1_000_000 / 8 // Mbit/s → bytes/s
	if err := applyBrutal(conn, rate, uint32(s.cfg.BrutalCwndGain)); err != nil {
		s.logger.Warn("tcp-brutal setsockopt failed; continuing with default CC",
			"remote", conn.RemoteAddr().String(), "err", err)
	}
}

// dialer returns a net.Dialer configured with the server's dial timeout,
// optional egress-interface binding, optional TFO connect, and the
// custom DNS resolver when `dns = …` is set.
func (s *Server) dialer() net.Dialer {
	d := net.Dialer{Timeout: s.cfg.DialTimeout}
	if s.resolver != nil {
		d.Resolver = s.resolver
	}
	var controls []func(string, string, syscall.RawConn) error
	if s.cfg.EgressInterface != "" {
		controls = append(controls, bindEgressInterface(s.cfg.EgressInterface))
	}
	if s.cfg.TFO {
		controls = append(controls, applyTFODial)
	}
	if c := chainListenControl(controls...); c != nil {
		d.Control = c
	}
	return d
}

// listenConfig returns a net.ListenConfig with optional egress-interface
// binding and optional TFO listen.
//
// Note: egress-interface here applies to the UDP-over-TCP listener and
// the QUIC mode listener (server's outbound-facing sockets); TFO applies
// to TCP listeners only. applyTFOListen no-ops for non-tcp networks, so
// it's safe to chain it on a ListenConfig that's reused for both.
func (s *Server) listenConfig() net.ListenConfig {
	lc := net.ListenConfig{}
	var controls []func(string, string, syscall.RawConn) error
	if s.cfg.EgressInterface != "" {
		controls = append(controls, bindEgressInterface(s.cfg.EgressInterface))
	}
	if s.cfg.TFO {
		controls = append(controls, applyTFOListen)
	}
	if c := chainListenControl(controls...); c != nil {
		lc.Control = c
	}
	return lc
}

// outboundNetwork returns the network family string to use for outbound
// dials of the given base ("tcp" or "udp"). When IPv6 is disabled in the
// server config, returns "tcp4" / "udp4" so Go's resolver only considers
// A records and Dial only attempts IPv4.
func (s *Server) outboundNetwork(base string) string {
	if !s.cfg.IPv6 {
		return base + "4"
	}
	return base
}

// ListenAndServe binds the configured address (TCP + optionally UDP for
// QUIC proxy mode) and serves until ctx is cancelled or a listener fails
// irrecoverably.
func (s *Server) ListenAndServe(ctx context.Context) error {
	lc := s.listenConfig()
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
	}

	// Optional QUIC proxy listener on the same address (UDP). Errors
	// from the QUIC side are logged but don't take down the TCP server,
	// so a misconfigured firewall blocking UDP doesn't break TCP relay.
	if s.cfg.QUIC {
		quicCtx, cancelQUIC := context.WithCancel(ctx)
		defer cancelQUIC()
		go func() {
			if qerr := s.ServeQUIC(quicCtx); qerr != nil {
				s.logger.Error("quic listener exited", "err", qerr)
			}
		}()
	}

	return s.ServeListener(ctx, ln)
}

// ServeListener accepts on ln until ctx is cancelled. Each connection is
// handed off to a fresh goroutine.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	s.logger.Info("snell server listening", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
		}
		s.maybeApplyBrutal(conn)
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	defer raw.Close()

	conn, err := obfs.NewObfsServer(raw, s.cfg.ObfsMode)
	if err != nil {
		s.logger.Warn("obfs server init failed", "err", err)
		return
	}

	var (
		stream *Snell
		user   string
	)

	if s.auth == nil {
		// FAST PATH (single-PSK): byte-for-byte equivalent to the
		// pre-multi-user code. No allocations, no LRU lookups, no
		// authentication branching. Do not refactor logic from below
		// into this branch without re-benchmarking.
		stream = ServerStreamConn(conn, s.psk)
	} else {
		// MULTI-USER PATH: read salt + first AEAD header from the
		// (post-obfs) stream and identify which user's PSK decrypts
		// it. On hit, hand the v4Reader a pre-derived AEAD plus the
		// 23 header bytes we consumed so the reader's nonce counter
		// can step through the same Open() naturally.
		result, aerr := s.auth.authenticate(conn, extractIP(raw.RemoteAddr()))
		if aerr != nil {
			if !errors.Is(aerr, io.EOF) {
				s.logger.Debug("snell auth failed",
					"remote", raw.RemoteAddr().String(),
					"err", aerr)
			}
			return
		}
		user = result.user.Name
		stream = ServerStreamConnPrefilled(conn, result.readAEAD, result.prefetchedHdr, result.user.PSK)
	}

	for {
		reuse, err := s.handleRequest(ctx, stream, user, raw.RemoteAddr())
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, ErrZeroChunk) {
				s.logger.Debug("snell request ended", "remote", raw.RemoteAddr().String(), "user", user, "err", err)
			}
			return
		}
		if !reuse {
			return
		}
	}
}

func (s *Server) handleRequest(ctx context.Context, stream *Snell, user string, remote net.Addr) (bool, error) {
	br := bufio.NewReader(stream)

	version, err := br.ReadByte()
	if err != nil {
		return false, err
	}
	if version != HeaderVersion {
		return false, fmt.Errorf("snell: invalid protocol version 0x%x", version)
	}

	command, err := br.ReadByte()
	if err != nil {
		return false, err
	}
	if command == CommandPing {
		if _, err := stream.Write([]byte{ResponsePong}); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := readClientID(br); err != nil {
		return false, err
	}

	switch command {
	case CommandConnect, CommandConnectV2:
		return s.handleTCP(ctx, stream, br, command == CommandConnectV2, user, remote)
	case CommandUDP:
		if !s.cfg.UDP {
			return false, writeServerError(stream, 1, "UDP disabled")
		}
		return false, s.handleUDP(ctx, stream)
	default:
		return false, fmt.Errorf("snell: unknown command 0x%x", command)
	}
}

func readClientID(r *bufio.Reader) (string, error) {
	length, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	id := make([]byte, int(length))
	if _, err := io.ReadFull(r, id); err != nil {
		return "", err
	}
	return string(id), nil
}

func (s *Server) handleTCP(ctx context.Context, stream *Snell, br *bufio.Reader, reuse bool, user string, remote net.Addr) (bool, error) {
	hostLen, err := br.ReadByte()
	if err != nil {
		return false, err
	}
	if hostLen == 0 {
		return false, errors.New("snell connect host is empty")
	}
	hostBytes := make([]byte, int(hostLen))
	if _, err := io.ReadFull(br, hostBytes); err != nil {
		return false, err
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(br, portBytes[:]); err != nil {
		return false, err
	}
	host := string(hostBytes)
	port := binary.BigEndian.Uint16(portBytes[:])
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))

	s.logger.Info("snell tcp",
		"remote", stream.Conn.RemoteAddr().String(),
		"target", target,
		"reuse", reuse,
		"user", user,
	)

	// Dial path: when the caller wired up OnAuthorize, defer to it.
	// They get the matched user, target, and a fully-configured dialer
	// (egress / DNS / TFO already baked in) and return either a (maybe
	// metering-wrapped) upstream conn, or a hard reject error.
	//
	// When OnAuthorize is nil — i.e., standalone OpenSnell or single-
	// user mode — we dial directly with the server's own dialer. No
	// behavior change from before multi-user support was added.
	dialer := s.dialer()
	var upstream net.Conn
	if s.cfg.OnAuthorize != nil {
		netw := s.outboundNetwork("tcp")
		upstream, err = s.cfg.OnAuthorize(ctx, ConnContext{
			User:       user,
			RemoteAddr: remote,
			Target:     target,
			Command:    CommandConnect,
			Reuse:      reuse,
		}, networkBindingDialer{base: dialer, network: netw})
	} else {
		upstream, err = dialer.DialContext(ctx, s.outboundNetwork("tcp"), target)
	}
	if err != nil {
		s.logger.Warn("upstream dial failed", "target", target, "err", err, "user", user)
		if werr := writeServerError(stream, errnoOf(err), err.Error()); werr != nil {
			return false, werr
		}
		return reuse, nil
	}
	defer upstream.Close()
	if tc, ok := upstream.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
	}

	// Lazy CONNECT response: instead of immediately writing the single
	// ResponseTunnel byte as its own frame, defer it until the first
	// real upstream→client data and merge them into one frame. Matches
	// Surge v5 server's Dynamic Record Sizing optimization (smaller and
	// more "natural-looking" first frame on the wire).
	lazy := &lazyResponseStream{Conn: stream}

	// Wrap the stream so the relay sees client's zero-chunk half-close as
	// io.EOF (instead of an error). This lets utils.Relay terminate
	// gracefully when the client sends its zero chunk.
	left := &serverReadConn{Conn: lazy, br: br}
	utils.Relay(left, upstream)

	// If the relay ended without the server ever writing anything (e.g.,
	// upstream connected and immediately closed without sending data),
	// the client is still waiting for the ResponseTunnel byte. Emit a
	// standalone-ResponseTunnel + zero-chunk frame pair so the client
	// sees a clean tunnel-established-and-EOF sequence.
	if !lazy.sent.Load() {
		if _, err := lazy.Write(nil); err != nil {
			return false, nil
		}
	}

	if !reuse {
		return false, nil
	}

	// reuse-mode cleanup. Reset any deadline set by Relay so we can talk
	// to the client one more time, then send our zero chunk and ensure
	// the client's zero chunk has been drained.
	_ = stream.Conn.SetReadDeadline(time.Time{})
	if _, err := lazy.Write(nil); err != nil {
		return false, nil
	}
	if !left.zeroChunkSeen {
		// Upstream closed before the client; drain until we see the
		// client's zero chunk so the next request starts on a clean
		// frame boundary.
		drain := pool.Get(pool.RelayBufferSize)
		defer func() { _ = pool.Put(drain) }()
		for {
			_, err := left.Read(drain)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return false, nil
			}
		}
	}
	return true, nil
}

// lazyResponseStream wraps the snell stream so the first non-error
// response byte (ResponseTunnel) is merged with the first frame of
// upstream→client relay data, instead of being sent as its own frame.
//
// Behavior on Write:
//   - First call with len(p) > 0: send AEAD(ResponseTunnel || p) in one
//     frame, return n=len(p) on success.
//   - First call with len(p) == 0: send ResponseTunnel as its own frame,
//     then forward the zero-chunk (half-close) frame.
//   - Subsequent calls: pass through unchanged.
//
// The status flag uses atomic.Bool because Write is called from the
// relay's goroutine and the caller may inspect `sent` from another
// goroutine to decide whether to emit a fallback at end-of-session.
type lazyResponseStream struct {
	net.Conn
	mu   sync.Mutex
	sent atomic.Bool
}

func (l *lazyResponseStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent.Load() {
		return l.Conn.Write(p)
	}
	l.sent.Store(true)

	if len(p) == 0 {
		// No upstream data ever flowed. Emit ResponseTunnel by itself,
		// then forward the requested zero chunk so the client sees a
		// proper half-close after the tunnel-established signal.
		if _, err := l.Conn.Write([]byte{ResponseTunnel}); err != nil {
			return 0, err
		}
		return l.Conn.Write(p)
	}

	merged := make([]byte, 1+len(p))
	merged[0] = ResponseTunnel
	copy(merged[1:], p)
	n, err := l.Conn.Write(merged)
	if err != nil {
		return 0, err
	}
	// The caller asked us to write len(p) bytes; we wrote 1+len(p).
	// Report back len(p) on success so io.Copy accounting stays sane.
	if n < len(merged) {
		// Partial write — translate proportionally, but the snell
		// frame writer always writes whole frames, so this branch is
		// effectively unreachable in practice.
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// serverReadConn wraps a *Snell + bufio.Reader so that:
//   - reads continue to flow through the bufio.Reader (preserving any
//     bytes it has already absorbed from the stream), and
//   - the snell zero-chunk half-close is surfaced to the relay as
//     io.EOF instead of ErrZeroChunk, allowing io.Copy to terminate
//     cleanly.
type serverReadConn struct {
	net.Conn
	br            *bufio.Reader
	zeroChunkSeen bool
}

func (b *serverReadConn) Read(p []byte) (int, error) {
	n, err := b.br.Read(p)
	if errors.Is(err, ErrZeroChunk) {
		b.zeroChunkSeen = true
		err = io.EOF
	}
	return n, err
}

func (s *Server) handleUDP(ctx context.Context, stream *Snell) error {
	s.logger.Info("snell udp session start", "remote", stream.Conn.RemoteAddr().String())
	if _, err := stream.Write([]byte{ResponseTunnel}); err != nil {
		return err
	}

	lc := s.listenConfig()
	pc, err := lc.ListenPacket(ctx, s.outboundNetwork("udp"), ":0")
	if err != nil {
		return writeServerError(stream, errnoOf(err), err.Error())
	}
	defer pc.Close()

	writeMu := &sync.Mutex{}
	ingressDone := make(chan struct{})
	go s.handleUDPIngress(stream, pc, writeMu, ingressDone)

	buf := pool.Get(MaxPayloadLength)
	defer func() { _ = pool.Put(buf) }()

	cache := newAddrCache(256)

	for {
		n, rerr := stream.Read(buf)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, ErrZeroChunk) {
				rerr = nil
			}
			_ = pc.Close()
			<-ingressDone
			return rerr
		}
		req, perr := ParseUDPRequest(buf[:n])
		if perr != nil {
			_ = pc.Close()
			<-ingressDone
			return perr
		}

		var target string
		if req.IP.IsValid() {
			target = net.JoinHostPort(req.IP.String(), strconv.Itoa(int(req.Port)))
		} else {
			target = net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))
		}

		uaddr, ok := cache.get(target)
		if !ok {
			resolved, rerr := net.ResolveUDPAddr("udp", target)
			if rerr != nil {
				s.logger.Warn("udp resolve failed", "target", target, "err", rerr)
				continue
			}
			cache.put(target, resolved)
			uaddr = resolved
		}

		s.logger.Debug("snell udp forward", "target", target, "payload", len(req.Payload))
		if _, werr := pc.WriteTo(req.Payload, uaddr); werr != nil {
			s.logger.Warn("udp write to remote failed", "target", target, "err", werr)
			break
		}
	}
	_ = pc.Close()
	<-ingressDone
	return nil
}

func (s *Server) handleUDPIngress(stream *Snell, pc net.PacketConn, writeMu *sync.Mutex, done chan<- struct{}) {
	defer close(done)
	buf := pool.Get(MaxPayloadLength)
	defer func() { _ = pool.Put(buf) }()

	for {
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("udp ingress error", "err", err)
			}
			return
		}
		s.logger.Debug("snell udp ingress", "from", raddr.String(), "len", n)
		writeMu.Lock()
		_, werr := WritePacketResponse(stream, raddr, buf[:n])
		writeMu.Unlock()
		if werr != nil {
			s.logger.Debug("udp response write failed", "err", werr)
			return
		}
	}
}

func writeServerError(w io.Writer, code byte, msg string) error {
	if len(msg) > 250 {
		msg = msg[:250]
	}
	buf := make([]byte, 0, 3+len(msg))
	buf = append(buf, ResponseError, code, byte(len(msg)))
	buf = append(buf, msg...)
	_, err := w.Write(buf)
	return err
}

func errnoOf(err error) byte {
	var sce syscall.Errno
	if errors.As(err, &sce) {
		return byte(sce)
	}
	return 0
}

// networkBindingDialer adapts the server's net.Dialer to the
// ContextDialer interface exposed in OnAuthorize, applying the server's
// IPv6/v4-only network override transparently so callers don't have to
// know about it. "tcp" / "udp" get rewritten to "tcp4" / "udp4" when
// cfg.IPv6 is false, matching the in-server dial path.
type networkBindingDialer struct {
	base    net.Dialer
	network string // already-resolved network family ("tcp", "tcp4", ...)
}

func (d networkBindingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// Honor whatever family the caller asked for, but if they pass the
	// generic "tcp" / "udp" we substitute the server-config-resolved
	// one (so an IPv6-disabled server stays IPv6-disabled).
	if network == "tcp" || network == "udp" {
		// Use the server-resolved family when it's strictly tighter
		// than the caller's; otherwise pass through.
		if d.network != "" && d.network != network {
			network = d.network
		}
	}
	return d.base.DialContext(ctx, network, addr)
}

// addrCache is a tiny LRU-ish cache. We do not need real LRU semantics for
// per-connection UDP destination resolution.
type addrCache struct {
	max   int
	mu    sync.Mutex
	store map[string]*net.UDPAddr
}

func newAddrCache(max int) *addrCache {
	return &addrCache{max: max, store: make(map[string]*net.UDPAddr, max)}
}

func (c *addrCache) get(k string) (*net.UDPAddr, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.store[k]
	return v, ok
}

func (c *addrCache) put(k string, v *net.UDPAddr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.store) >= c.max {
		for evict := range c.store {
			delete(c.store, evict)
			if len(c.store) < c.max {
				break
			}
		}
	}
	c.store[k] = v
}
