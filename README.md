# OpenSnell (alpha branch)

English | [简体中文](README_zh.md)

> **You are reading the `alpha` branch README.** This branch tracks
> [`main`](https://github.com/missuo/opensnell/tree/main) and layers
> experimental features that the official Surge `snell-server` does NOT
> ship — right now that means `tcp-brutal` congestion control. If you only
> need Surge-compatible behavior, use the `main` branch and its tagged
> releases. Use the `alpha` branch if you specifically want the extra
> features documented here.

A Go implementation of the Snell proxy protocol, versions **4** and **5** —
server-side and client-side, with **end-to-end interoperability against the
official Surge `snell-server v5.0.1`** verified for every code path below.

Snell v5's UDP/QUIC proxy mode is supported on the **server** only; pair it
with the **Surge** client (or any other v5-capable client) when you need
HTTP/3 acceleration for downstream applications.

### Why no v1 / v2 / v3?

This project deliberately drops support for the older Snell protocols.
Their stream framing predates the v4 padding/AEAD redesign and is at this
point trivially fingerprintable on the wire — in particular, traffic
patterns of v1/v2/v3 no longer reliably traverse the GFW and they
generally are not recommended for new deployments. If you have a legacy
v1/v2 setup you cannot retire yet, the sibling project
[open-snell](https://github.com/icpz/open-snell) (and its forks) still
implements those versions; this codebase focuses on the v4/v5 wire that
the current Surge `snell-server` speaks.

## Feature matrix

| Path                                  | `snell-server` | `snell-client` |
| ------------------------------------- | -------------- | -------------- |
| TCP CONNECT                           | ✅             | ✅             |
| TCP CONNECT with reuse (`CommandConnectV2`) | ✅       | ✅             |
| UDP-over-TCP (snell datagram)         | ✅             | ✅             |
| `http` / `tls` obfs                   | ✅             | ✅             |
| Dynamic Record Sizing (v5)            | ✅             | ✅             |
| `egress-interface` (v5)               | ✅             | —              |
| `ipv6` outbound family toggle (v5)    | ✅             | —              |
| Custom upstream DNS (`dns = …`)       | ✅             | —              |
| TCP Fast Open (Linux only)            | ✅             | ✅             |
| **QUIC proxy mode (v5)**              | ✅             | use Surge      |
| **`tcp-brutal` CC (experimental, Linux only)** | ✅    | ✅             |

## Install

### One-line server installer (Linux + systemd)

```sh
bash <(curl -fsSL https://s.ee/opensnell)
```

The interactive installer:

- Lets you pick between **OpenSnell** (default, GPLv3, all-platform) or
  the **official Surge `snell-server v5.0.1`** (closed-source, Linux only).
- Generates a random PSK with `openssl` if you leave it blank.
- Picks an unused random port in `10000–60000` if you leave the port blank.
- Writes `/etc/snell/snell-server.conf`, installs a systemd unit
  (`snell-server.service`), opens the port in UFW / firewalld if either
  is active, and starts the service.
- Re-run with `reconfigure`, `update`, `uninstall`, `start`, `stop`,
  `restart`, `status`, or `info` — see `./install.sh help`.

### Build from source

```sh
go build -o snell-server ./cmd/snell-server
go build -o snell-client ./cmd/snell-client
```

Or fetch directly:

```sh
go install github.com/missuo/opensnell/cmd/snell-server@latest
go install github.com/missuo/opensnell/cmd/snell-client@latest
```

## Server configuration

`snell-server.conf` — pass via `-c <path>`. All keys live under the
`[snell-server]` section.

```ini
[snell-server]

; Bind address(es). Required. Set to 0.0.0.0:<port> to accept from
; anywhere, or 127.0.0.1:<port> when fronted by another reverse proxy.
; When `quic = true` (default) the server also listens for UDP on the
; same port for QUIC proxy mode, so make sure both TCP/<port> and
; UDP/<port> are open in any firewall in front of the host.
listen = 0.0.0.0:2333

; Pre-shared key. Required. Treated as a raw UTF-8 string (NOT base64
; decoded) — keep this exactly as configured on the client side.
psk = your-shared-secret

; Obfuscation layer wrapping the snell stream. Optional, default off.
;   off  — no obfuscation (recommended; the v4/v5 frame format already
;          uses per-frame padding for traffic-shape obfuscation)
;   http — fake HTTP/1.1 Upgrade handshake
;   tls  — fake TLS ClientHello/ServerHello handshake
obfs = off

; Accept UDP-over-TCP from clients (snell's own datagram-in-stream
; protocol; distinct from QUIC mode below). Optional, default true.
udp = true

; QUIC proxy mode (v5). Optional, default true. When enabled, the
; server additionally listens on UDP/<port> from `listen` and accepts
; snell-encrypted envelopes that wrap a QUIC Initial packet; once the
; (src_ip, src_port) → upstream mapping is established, all subsequent
; UDP packets are forwarded as raw QUIC in both directions. Required
; for HTTP/3 acceleration with Surge clients that have `block-quic=off`.
quic = true

; Outbound interface binding. Optional, default empty (use the host's
; default routing). When set, all upstream sockets (TCP dials and
; UDP-over-TCP listeners and QUIC upstream dials) are pinned to this
; interface via SO_BINDTODEVICE on Linux or IP_BOUND_IF on macOS.
; Other platforms reject this at dial time.
egress-interface =

; Whether outbound dials may use IPv6 destinations. Optional, default
; true (matching the official Surge snell-server's `ipv6 = true`).
; When false, the dialer is constrained to "tcp4" / "udp4" — Go's
; resolver only considers A records and AAAA lookups are skipped.
; Useful on hosts whose IPv6 path is broken or slow. Only affects
; outbound; what addresses the server LISTENS on is still controlled
; by `listen` (write `[::]:2333` for v6 dual-stack inbound).
ipv6 = true

; Comma-separated list of upstream DNS servers used to resolve
; client-requested hostnames. Optional, default empty (use the host's
; system resolver via /etc/resolv.conf). Each entry is an IP literal
; (v4 or v6) with an optional `:port` suffix; if no port is given, 53
; is assumed. Servers are tried in order on each lookup. Matches the
; official Surge snell-server `dns = …` directive added in v4.1.0.
; Each configured server is logged at startup as
;   level=INFO msg="effective DNS" server=<addr>
dns =

; TCP Fast Open (RFC 7413). Optional, default false. When enabled,
; both the inbound TCP listener and outbound upstream TCP dials get
; TFO setsockopt, allowing the snell client's first data write to
; ride along in the SYN packet — saving one RTT per fresh TCP
; connection. Linux only (uses TCP_FASTOPEN / TCP_FASTOPEN_CONNECT).
; Requires the kernel sysctl `net.ipv4.tcp_fastopen` to have bit 1
; set for server (run `sysctl -w net.ipv4.tcp_fastopen=3`). On other
; platforms this option is a silent no-op.
tfo = false

; --- EXPERIMENTAL: tcp-brutal congestion control ---
; Switches every accepted TCP connection from a snell client onto the
; apernet/tcp-brutal kernel CC algorithm and pins it to `brutal-mbps`.
; Linux only; requires `apt install linux-headers-$(uname -r) && cd
; tcp-brutal && make && make load` first. If the kernel module is
; missing the server just logs a warning and falls back to default CC.
;
; ⚠️  WARNING: brutal applies its rate PER TCP CONNECTION. Snell has no
; native mux, so multiple concurrent SOCKS5 sessions each get the full
; rate and will overwhelm the receiver. Only safe for single-stream
; workloads, or paired with `reuse = true` and known-low concurrency.
brutal = false
brutal-mbps = 100         ; required when brutal = true; per-conn rate in Mbit/s
brutal-cwnd-gain = 15     ; optional; tenths (15 = 1.5x, 20 = 2.0x)
```

Run:

```sh
./snell-server -c snell-server.conf       # info level logs
./snell-server -c snell-server.conf -v    # debug level logs
```

A minimal systemd unit might look like:

```ini
[Unit]
Description=OpenSnell server
After=network.target

[Service]
ExecStart=/usr/local/bin/snell-server -c /etc/snell/snell-server.conf
Restart=on-failure
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

## Client configuration

`snell-client.conf` exposes a local **SOCKS5** proxy (TCP CONNECT plus
UDP ASSOCIATE) and tunnels every accepted request through a snell server.
For QUIC/HTTP-3 use Surge as the front-end — this client is for tools
that already speak SOCKS5 (`curl --socks5-hostname`, browser proxy
settings, application SOCKS5 hooks, etc.).

```ini
[snell-client]

; Local SOCKS5 listener. Required. Bind to 127.0.0.1 unless you really
; mean to expose the proxy to the LAN.
listen = 127.0.0.1:1080

; Remote snell server, host:port. Required.
server = your-server.example.com:2333

; Pre-shared key, must match the server's `psk` byte-for-byte.
psk = your-shared-secret

; Snell protocol version this client claims to be. Optional, default v5.
;   v4 — explicit v4 client
;   v5 — explicit v5 client (recommended)
; v4 and v5 share the same TCP wire format, so this is informational
; today (logged at startup). The Surge v5 server is documented as
; backward-compatible with v4 clients.
version = v5

; Obfuscation layer. Optional, default off. Must match the server's
; setting. Valid values: off | http | tls.
obfs = off

; Host header / SNI used by the http/tls obfs layer. Optional, default
; is to reuse the server hostname.
obfs-host = bing.com

; Reuse upstream TCP connections across multiple SOCKS5 requests
; (snell's `CommandConnectV2`). Optional, default false. Recommended on
; for short HTTP requests; the pool caps each TCP at 2 sessions to
; match the real Surge server's behavior and drains the server's
; half-close zero chunk before putting a connection back so the next
; reuse starts on a clean frame boundary.
reuse = true

; TCP Fast Open on outbound dials to the snell server. Optional,
; default false. Linux only — see the server-side `tfo` notes above
; for the kernel sysctl requirements.
tfo = false

; --- EXPERIMENTAL: tcp-brutal congestion control on outbound ---
; Switches every TCP connection to the snell server onto the brutal CC
; algorithm. Linux only; this only controls the CLIENT→SERVER
; (upload) direction — for download-side acceleration set the same
; option on the server. Same multiplexing caveat as the server-side
; option applies. Off by default.
brutal = false
brutal-mbps = 100         ; required when brutal = true; per-conn upload rate
brutal-cwnd-gain = 15     ; optional; tenths
```

Run:

```sh
./snell-client -c snell-client.conf       # info level logs
./snell-client -c snell-client.conf -v    # debug level logs
```

### Example end-to-end smoke test

```sh
# In another terminal, with snell-client running on 127.0.0.1:1080:
curl -sS --socks5-hostname 127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace
# Expect a body whose `ip=` line shows the snell-server's egress IP.
```

## Using OpenSnell server with Surge (recommended for QUIC/HTTP-3)

In your Surge config, add the server as a snell proxy with `version=5`
and disable Surge's per-connection QUIC block:

```ini
[Proxy]
my-snell = snell, your-server.example.com, 2333, psk=your-shared-secret, version=5, tfo=true, block-quic=off
```

When Surge dispatches an HTTP/3 connection through `my-snell`, it wraps
the first 1–2 QUIC Initial packets in the snell envelope (containing
the target SNI/host so it's hidden on the wire) and then forwards the
rest as raw QUIC — handled by `snell-server`'s `ServeQUIC` loop.

## Protocol notes

### TCP frame layout (v4 / v5)

- **Key derivation**: `argon2id(psk_utf8, salt, t=3, m=8 KiB, p=1)` →
  32 bytes; the first 16 are the AES-128-GCM key.
- Per-direction 16-byte random salt, sent once before the first frame.
- Each frame: a 7-byte plaintext header
  `[type=4, 0, 0, padLen_be, payloadLen_be]` is AEAD-sealed (nonce=N),
  followed by `padLen` bytes of padding and an AEAD-sealed payload
  (nonce=N+1). The nonce counter is a 12-byte little-endian increment.
- Bytes at even indices of the padding region are swapped with the
  leading bytes of the payload ciphertext (see `swapPadding`), so the
  raw padding bytes never appear contiguously on the wire.
- The first frame on every stream carries an extra `0x100..0x1FF`-byte
  padding chosen so the overall ones/zeros ratio of
  salt+padding+ciphertext stays within a "natural" range; later frames
  scale the maximum payload up from a small initial size to
  `MaxPayloadLength`, then reset after a 30 s idle window (this is
  v5's **Dynamic Record Sizing** optimization).

### QUIC envelope layout (v5 only, client → server, first packet of a flow)

```
[salt(16B random)]
[AEAD-Seal(K, nonce=0, [0x04, 0, 0, padLen_be, payloadLen_be]) || 16B tag]
[padding(padLen)]
[AEAD-Seal(K, nonce=1, request_header || inner_QUIC_packet) || 16B tag]

request_header = [0x01, 0x01, 0x00, hostlen, host, port_be]
K              = Argon2id(psk_utf8, salt, 3, 8 KiB, 1, 32)[:16]
AEAD           = AES-128-GCM
```

After the server decodes the first envelope and records the
`(client_src, upstream)` mapping, every subsequent UDP packet in either
direction is forwarded as **raw QUIC** with no additional snell framing.

This format was reverse-engineered against the official Surge
`snell-server v5.0.1` by capturing real HTTP/3 traffic from a Surge
client and decrypting with the configured PSK; see
`components/snell/quic.go` and `components/snell/quic_test.go` (the
unit test includes a real captured 1359-byte envelope as a fixture).

## Performance

We benchmarked OpenSnell against the official `snell-server v5.0.1` (the
same closed-source binary that ships behind the Surge client) on two
co-located Linux hosts: one host runs **both** servers on different
ports so the upstream link, kernel, and CDN cooldown apply equally;
the other host runs two snell-client instances pointed at each. All
traffic goes through SOCKS5 via `curl --socks5-hostname` to the same
upstream URL.

### Method

Three phases, each run sequentially (never simultaneously) with a
several-second pause between subjects so the upstream CDN doesn't
throttle one side of the comparison:

1. **Latency** — 50 sequential requests to a tiny endpoint
   (`cloudflare.com/cdn-cgi/trace`, ~200 B response). Measure
   `time_connect`, TTFB, and total via `curl -w`.
2. **Concurrent throughput** — N = 2, 4, 8 parallel downloads of a
   10 MB file. Measure aggregate MB/s = total bytes ÷ wall clock.
3. **Packet inspection** — `tcpdump` server-side during a single
   10 MB download per variant; count full TCP segments vs. empty ACKs.

### What the official binary actually is

We disassembled the official `snell-server-v5.0.1-linux-amd64`
(1.2 MB, statically linked, section headers stripped). String
analysis shows it is built with **GCC**, links **libuv** (the same
async-I/O library curl and Node.js use), and uses **OpenSSL**'s
AES-NI GCM implementation (the distinctive `GCM module for x86_64`
string is present). In short: **C/C++ + libuv + OpenSSL**. That
matters because libuv runs the whole proxy on a single event-loop
thread — no per-connection goroutine, no GMP scheduling, no GC.

### Initial finding (OpenSnell v1.0.1)

| Metric                                       | OpenSnell v1.0.1 | Official v5.0.1 | Δ           |
| -------------------------------------------- | ---------------: | --------------: | ----------- |
| TTFB median                                  |       within noise |   within noise | ~0          |
| Single-stream throughput                     |               tied |            tied | ~0          |
| **N = 8 concurrent throughput**              |       **6.49 MB/s** |  **8.46 MB/s** | **−30 %**   |
| Empty ACKs over a 10 MB transfer             |              1444 |            1084 | **+33 %**   |

Single-stream and latency were already on par with the official
server. The gap was concentrated in concurrent throughput.

### Root cause

`v4Reader.readFrame()` deserialises every snell frame with **two
distinct `io.ReadFull` calls** — one for the 23-byte AEAD'd frame
header, one for padding + payload + tag — and the underlying
`net.Conn` was being read directly, with no userspace buffering. At a
typical frame size of ~1.5 KB, a 10 MB transfer touches ~7300 frames
and therefore costs **~14 000 `recv()` syscalls per direction**.

Two things follow from that:

1. **Empty ACKs.** Linux delays ACKs when an application drains the
   receive buffer in big bursts, but issues them more aggressively
   when the buffer is drained through many small reads. Two syscalls
   per frame == many small reads == defeat delayed-ACK == ~33 % more
   empty ACKs on the wire than the C reference.
2. **Concurrent throughput.** Each snell connection runs two
   goroutines (one per direction). At N = 8 concurrent SOCKS5 sessions
   that is 16 goroutines, each doing thousands of small syscalls and
   trading off through Go's runtime scheduler. libuv pays none of that
   — its single epoll-driven thread can absorb new TCP data at full
   rate.

### Fix

One line:

```go
// components/snell/v4.go — initReader()
c.r = &v4Reader{Reader: bufio.NewReaderSize(c.Conn, 64*1024), aead: aead}
```

A 64 KB read-side buffer pulls ~40 max-sized snell frames into
userspace per `recv()`, cutting syscalls on the read path by roughly
~90×. This is a wire-format-transparent change: the v4 frame parser
still sees the exact same byte stream, just delivered through fewer
syscalls.

### After OpenSnell v1.0.2

| Metric                                       | OpenSnell v1.0.2 | Official v5.0.1 | Δ           |
| -------------------------------------------- | ---------------: | --------------: | ----------- |
| TTFB median                                  |          17.9 ms |        17.1 ms  | +4.7 %      |
| TTFB p95                                     |          25.4 ms |        24.5 ms  | +3.7 %      |
| N = 2 throughput                             |      43.48 MB/s  |    44.44 MB/s   | −2.2 %      |
| **N = 8 throughput**                         |   **47.34 MB/s** |  **48.19 MB/s** | **−1.8 %**  |
| Empty ACKs over a 10 MB transfer             |             2596 |           2343  | **+10.8 %** |

The concurrent throughput gap collapsed from **−30 %** to **−1.8 %**,
and the empty-ACK excess dropped from **+33 %** to **+10.8 %**. The
remaining ~11 % ACK excess and ~2 % throughput delta is plausibly
attributable to Go's runtime overhead vs. a hand-written C event
loop — and below the noise floor of any realistic workload.

### Takeaway

On Surge's published wire (snell v5), OpenSnell's `snell-server`
runs at **roughly 98 % of the official C reference under concurrency**
and is **indistinguishable in latency**. The bufio fix is `+9/−1`
lines in `components/snell/v4.go` — a useful reminder that profiling
the read path (and not just application logic) is where most of the
gap to a native C/libuv implementation lives.

## Experimental: tcp-brutal congestion control

[apernet/tcp-brutal](https://github.com/apernet/tcp-brutal) is a Linux
kernel module that exposes a `brutal` TCP congestion-control algorithm.
Userspace sets a fixed per-socket sending rate; the kernel paces the
socket to that rate without listening to loss feedback. Useful on
high-loss long-fat paths where cubic/bbr collapse.

How to use with OpenSnell:

```sh
# On the server host (Linux):
apt install linux-headers-$(uname -r) make gcc
git clone https://github.com/apernet/tcp-brutal /tmp/tcp-brutal
cd /tmp/tcp-brutal && make && insmod brutal.ko
# Verify it loaded:
cat /proc/sys/net/ipv4/tcp_available_congestion_control   # should now include "brutal"
```

Then set `brutal = true` and `brutal-mbps = <Mbps>` in
`snell-server.conf` (and/or `snell-client.conf`). Restart and the
relevant direction will be rate-pinned per connection.

**Verified end-to-end** on a v6-capable Linux server (`brutal-mbps = 50`,
100 MB download via `cachefly.cachefly.net/100mb.test`):

| Server CC      | Time   | Speed     |
| -------------- | ------ | --------- |
| vanilla (cubic)| 1.89 s | 444 Mbps  |
| brutal (50)    | 16.97 s| **49.4 Mbps**(< 2 % off the configured 50)|

### ⚠️ Read this before turning brutal on

Quoting the upstream maintainers verbatim:

> "Brutal's rate setting **applies to each individual connection**.
> This makes it suitable only for protocols that support multiplexing
> (mux). For protocols that require a separate connection for each
> proxy connection, using TCP Brutal will **overwhelm the receiver**
> if multiple connections are active at the same time."

Snell does **not** support mux. Reuse-mode (`CommandConnectV2`) is
*serial* — one CONNECT per TCP at a time — and the client may still
hold multiple pooled TCPs concurrently. **If your workload opens N
parallel snell connections, your server tries to push N × `brutal-mbps`
total** and the receiver may collapse with packet loss. Use this only
when:

- You have a sustained single-stream workload (large file download,
  video stream, etc.), OR
- You're testing.

Either side can enable brutal independently. Server-only enable
controls server → client (downloads — the typical proxy traffic);
client-only enable controls client → server (uploads). They don't
need to match.

## Interop with the real Surge `snell-server`

Tested against `snell-server v5.0.1 (Nov 19 2025)`:

| Path                                | Result                                       |
| ----------------------------------- | -------------------------------------------- |
| Our client → real server, TCP       | ✅ 10/10                                      |
| Our client → real server, UDP-over-TCP | ✅ DNS round-tripped                       |
| Our client → real server, reuse     | ✅ 30 sequential + 20 parallel                |
| Our server, QUIC mode, real Surge envelope | ✅ unit test on a real capture          |
| HTTP/3 → our server → Cloudflare    | ✅ 5/5 (`ip=` echoes our server, `sni=plaintext`) |

## References

- [MetaCubeX/mihomo#2816](https://github.com/MetaCubeX/mihomo/pull/2816) —
  earlier reverse-engineered Snell v5 proposal (closed in favor of #2817);
  the description of the AEAD frame layout and padding interleave was the
  starting point for this implementation.
- [MetaCubeX/mihomo#2817](https://github.com/MetaCubeX/mihomo/pull/2817) —
  merged Snell v4/v5 outbound + inbound for mihomo; the TCP protocol
  layer here is a port of that code, adapted into a standalone
  server/client and stripped of v1/v2/v3 support.
- [Surge snell release notes](https://kb.nssurge.com/surge-knowledge-base/release-notes/snell) —
  upstream's published feature list per release.

## License

GPLv3 — see [LICENSE.md](LICENSE.md). Portions of the obfs, socks5, and
buffer-pool code originate from the open-snell / clash projects.
