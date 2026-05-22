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
	"sync"
	"syscall"
	"time"

	"github.com/missuo/opensnell/components/obfs"
	"github.com/missuo/opensnell/components/utils"
	"github.com/missuo/opensnell/components/utils/pool"
)

// ServerConfig is the runtime configuration for Server.
type ServerConfig struct {
	Listen   string // host:port to bind on
	PSK      string // shared pre-shared key
	ObfsMode string // "", "off", "http", "tls"
	UDP      bool   // accept UDP-over-TCP requests
	DialTimeout time.Duration
}

// Server is a snell v4/v5 server. Use NewServer + Serve, or pass an
// accepted net.Listener to ServeListener if you want to manage the socket
// yourself.
type Server struct {
	cfg    ServerConfig
	psk    []byte
	logger *slog.Logger
}

func NewServer(cfg ServerConfig, logger *slog.Logger) (*Server, error) {
	if cfg.PSK == "" {
		return nil, errors.New("snell server requires psk")
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
	return &Server{cfg: cfg, psk: []byte(cfg.PSK), logger: logger}, nil
}

// ListenAndServe binds the configured address and serves until ctx is
// cancelled or the listener fails irrecoverably.
func (s *Server) ListenAndServe(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
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
	stream := ServerStreamConn(conn, s.psk)

	for {
		reuse, err := s.handleRequest(ctx, stream)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, ErrZeroChunk) {
				s.logger.Debug("snell request ended", "remote", raw.RemoteAddr().String(), "err", err)
			}
			return
		}
		if !reuse {
			return
		}
	}
}

func (s *Server) handleRequest(ctx context.Context, stream *Snell) (bool, error) {
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
		return s.handleTCP(ctx, stream, br, command == CommandConnectV2)
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

func (s *Server) handleTCP(ctx context.Context, stream *Snell, br *bufio.Reader, reuse bool) (bool, error) {
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
	)

	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	upstream, derr := dialer.DialContext(ctx, "tcp", target)
	if derr != nil {
		if werr := writeServerError(stream, errnoOf(derr), derr.Error()); werr != nil {
			return false, werr
		}
		return reuse, nil
	}
	defer upstream.Close()
	if tc, ok := upstream.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
	}

	if _, err := stream.Write([]byte{ResponseTunnel}); err != nil {
		return false, err
	}

	// Wrap the stream so the relay sees client's zero-chunk half-close as
	// io.EOF (instead of an error). This lets utils.Relay terminate
	// gracefully when the client sends its zero chunk.
	left := &serverReadConn{Conn: stream, br: br}
	utils.Relay(left, upstream)

	if !reuse {
		return false, nil
	}

	// reuse-mode cleanup. Reset any deadline set by Relay so we can talk
	// to the client one more time, then send our zero chunk and ensure
	// the client's zero chunk has been drained.
	_ = stream.Conn.SetReadDeadline(time.Time{})
	if _, err := stream.Write(nil); err != nil {
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

	pc, err := net.ListenPacket("udp", ":0")
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
