/*
 * This file is part of opensnell.
 * SPDX-License-Identifier: GPL-3.0-or-later
 */

package snell

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/missuo/opensnell/components/obfs"
	"github.com/missuo/opensnell/components/socks5"
	"github.com/missuo/opensnell/components/utils"
	"github.com/missuo/opensnell/components/utils/pool"
)

// ClientConfig configures a client-side snell dialer.
type ClientConfig struct {
	Server      string // host:port of remote snell server
	PSK         string
	ObfsMode    string
	ObfsHost    string // SNI/Host header for http/tls obfs
	Reuse       bool
	DialTimeout time.Duration

	// Version selects the protocol variant the client logs/announces. It
	// is "v4" or "v5"; default is "v5". Empty maps to "v5".
	//
	// Today this is informational only — v4 and v5 share the same TCP
	// wire format (same AES-128-GCM + Argon2id KDF, same frame layout)
	// and the v5 server's documented backward compatibility means a v4
	// client connects to a v5 server transparently. The field is wired
	// up now so a future QUIC-mode implementation can switch on it
	// without breaking config compatibility.
	Version string
}

func (c ClientConfig) normalizedVersion() string {
	switch c.Version {
	case "", "v5", "V5", "5":
		return "v5"
	case "v4", "V4", "4":
		return "v4"
	default:
		return c.Version
	}
}

// Client dials snell servers and exposes a local SOCKS5 entry point.
type Client struct {
	cfg       ClientConfig
	psk       []byte
	host      string
	port      string
	pool      *Pool
	logger    *slog.Logger
}

func NewClient(cfg ClientConfig, logger *slog.Logger) (*Client, error) {
	if cfg.Server == "" {
		return nil, errors.New("snell client requires server")
	}
	if cfg.PSK == "" {
		return nil, errors.New("snell client requires psk")
	}
	switch cfg.ObfsMode {
	case "", "off", "http", "tls":
	default:
		return nil, fmt.Errorf("snell client: unknown obfs mode %q", cfg.ObfsMode)
	}
	switch cfg.normalizedVersion() {
	case "v4", "v5":
	default:
		return nil, fmt.Errorf("snell client: unsupported version %q (use v4 or v5)", cfg.Version)
	}
	host, port, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("snell client: invalid server %q: %w", cfg.Server, err)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 8 * time.Second
	}
	if cfg.ObfsHost == "" {
		cfg.ObfsHost = host
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("snell client",
		"server", cfg.Server,
		"version", cfg.normalizedVersion(),
		"reuse", cfg.Reuse,
		"obfs", cfg.ObfsMode,
	)

	c := &Client{cfg: cfg, psk: []byte(cfg.PSK), host: host, port: port, logger: logger}
	if cfg.Reuse {
		c.pool = NewPool(c.dialFresh)
	}
	return c, nil
}

// dialFresh opens a new TCP+obfs+snell connection to the server, with no
// CONNECT header written yet.
func (c *Client) dialFresh(ctx context.Context) (*Snell, error) {
	dialer := net.Dialer{Timeout: c.cfg.DialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", c.cfg.Server)
	if err != nil {
		return nil, err
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
	}
	obfsConn, err := obfs.NewObfsClient(raw, c.cfg.ObfsHost, c.port, c.cfg.ObfsMode)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return StreamConn(obfsConn, c.psk), nil
}

// DialTCP opens a snell TCP connection to (host, port).
//
// Reuse-aware: tries the pool first; if the pool yields a connection but
// writing the request header fails (server already closed the TCP socket
// between sessions, broken pipe), the stale conn is discarded and a fresh
// one is dialed. The caller never sees the pool's bookkeeping.
//
// We can't retry transparently *after* the conn is exposed to a relay,
// because by then bytes from the local side (e.g., a TLS ClientHello) may
// have already been pushed to a now-dead socket. So the only safe retry
// point is before WriteHeader returns. The first-read EOF case is handled
// instead by the per-connection use cap in the pool (see Pool.Get).
func (c *Client) DialTCP(ctx context.Context, host string, port uint16) (net.Conn, error) {
	if len(host) > 255 {
		return nil, errors.New("snell client: host name too long")
	}

	if c.pool != nil {
		for attempts := 0; attempts < 2; attempts++ {
			conn, perr := c.pool.GetContext(ctx)
			if perr != nil {
				break
			}
			if werr := WriteHeader(conn, host, port, true); werr != nil {
				c.logger.Debug("pool conn write failed, retry", "attempt", attempts, "err", werr)
				_ = conn.Close()
				continue
			}
			return conn, nil
		}
	}

	conn, err := c.dialFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := WriteHeader(conn, host, port, c.cfg.Reuse); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// DialUDP opens a snell UDP-associate connection. Returns a net.PacketConn
// the caller can use to ferry datagrams. The returned PacketConn must be
// closed by the caller.
func (c *Client) DialUDP(ctx context.Context) (net.PacketConn, error) {
	conn, err := c.dialFresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := WriteUDPHeader(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Drain server's ResponseTunnel byte before exposing packetConn.
	if err := conn.ReadReply(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return PacketConn(conn), nil
}

// ServeSOCKS5 listens on listenAddr and forwards SOCKS5 CONNECT and
// UDP-ASSOCIATE requests through the snell server.
func (c *Client) ServeSOCKS5(ctx context.Context, listenAddr string) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return err
	}
	c.logger.Info("SOCKS5 proxy listening", "addr", ln.Addr().String())

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
		go c.handleSocks(ctx, conn)
	}
}

func (c *Client) handleSocks(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	addr, cmd, err := socks5.ServerHandshake(conn)
	if err != nil {
		c.logger.Debug("SOCKS handshake failed", "remote", conn.RemoteAddr().String(), "err", err)
		return
	}

	switch cmd {
	case socks5.CmdConnect:
		c.handleSocksConnect(ctx, conn, addr)
	case socks5.CmdUDPAssociate:
		c.handleSocksUDP(ctx, conn, addr)
	default:
		c.logger.Debug("SOCKS command not supported", "cmd", cmd)
	}
}

func (c *Client) handleSocksConnect(ctx context.Context, conn net.Conn, addr socks5.Addr) {
	host, port := addr.HostPort()
	c.logger.Info("snell tcp",
		"remote", conn.RemoteAddr().String(),
		"target", net.JoinHostPort(host, strconv.Itoa(int(port))),
	)

	upstream, err := c.DialTCP(ctx, host, port)
	if err != nil {
		c.logger.Warn("snell dial failed", "target", addr.String(), "err", err)
		return
	}
	defer upstream.Close()

	utils.Relay(conn, upstream)
}

// handleSocksUDP wires a SOCKS5 UDP relay socket through to the snell
// server. Per RFC 1928, the SOCKS5 TCP control channel staying open is
// what authorizes the UDP relay — so we keep the TCP conn open and tear
// the whole thing down when it closes.
func (c *Client) handleSocksUDP(ctx context.Context, ctrl net.Conn, _ socks5.Addr) {
	udpLn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		c.logger.Warn("local UDP listen failed", "err", err)
		return
	}
	defer udpLn.Close()

	// rewrite the BND.ADDR/BND.PORT we sent during handshake to the actual
	// UDP-listener port. The handshake reply already went out using the
	// TCP local address; some SOCKS5 clients ignore the port and reuse the
	// TCP host, others honor the port. We let the client deal with it; the
	// common case (curl --socks5-hostname / Surge) is that the client uses
	// the TCP socket's host with the original BND port.

	snellPC, err := c.DialUDP(ctx)
	if err != nil {
		c.logger.Warn("snell udp dial failed", "err", err)
		return
	}
	defer snellPC.Close()

	// Cache the first source address from the SOCKS5 client so we know
	// where to send replies. (Multiple sources from the same TCP control
	// connection are unusual but supported.)
	var clientAddrMu sync.Mutex
	var clientAddr net.Addr

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	// Pump 1: SOCKS5 client -> snell server
	go func() {
		defer cancel()
		buf := pool.Get(pool.RelayBufferSize)
		defer func() { _ = pool.Put(buf) }()
		for {
			if err := udpLn.SetReadDeadline(time.Now().Add(5 * time.Minute)); err != nil {
				return
			}
			n, src, err := udpLn.ReadFrom(buf)
			if err != nil {
				return
			}
			addr, payload, err := socks5.DecodeUDPPacket(buf[:n])
			if err != nil {
				continue
			}
			clientAddrMu.Lock()
			clientAddr = src
			clientAddrMu.Unlock()

			ua := addr.UDPAddr()
			if ua == nil {
				// snell wire format also accepts domain names — we punt
				// these by resolving locally. The server resolves
				// AtypDomainName itself, but the snell client packet
				// helpers don't currently have a domain-aware path; resolve
				// here for simplicity.
				ua, err = net.ResolveUDPAddr("udp", addr.String())
				if err != nil {
					continue
				}
			}
			if _, err := snellPC.WriteTo(payload, ua); err != nil {
				return
			}
		}
	}()

	// Pump 2: snell server -> SOCKS5 client
	go func() {
		defer cancel()
		buf := pool.Get(pool.RelayBufferSize)
		defer func() { _ = pool.Put(buf) }()
		for {
			n, raddr, err := snellPC.ReadFrom(buf)
			if err != nil {
				return
			}
			clientAddrMu.Lock()
			dst := clientAddr
			clientAddrMu.Unlock()
			if dst == nil {
				continue
			}
			socksAddr := socks5.ParseAddrToSocksAddr(raddr)
			frame, err := socks5.EncodeUDPPacket(socksAddr, buf[:n])
			if err != nil {
				continue
			}
			if _, err := udpLn.WriteTo(frame, dst); err != nil {
				return
			}
		}
	}()

	// Pump 3: TCP control channel — when client closes, tear down.
	go func() {
		defer cancel()
		discard := pool.Get(pool.RelayBufferSize)
		defer func() { _ = pool.Put(discard) }()
		for {
			_, err := ctrl.Read(discard)
			if err != nil {
				return
			}
		}
	}()

	<-ctx2.Done()
}
