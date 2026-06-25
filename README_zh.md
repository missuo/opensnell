# OpenSnell

[English](README.md) | 简体中文

OpenSnell 是 Snell 代理协议 **v4** 和 **v5** 的 Go 实现，包含服务端与
客户端。下文列出的所有路径都已经过验证，可与官方 Surge `snell-server v5.0.1`
端到端互通。

Snell v5 的 UDP/QUIC 代理模式目前只在**服务端**实现；如果要为下游应用启用
HTTP/3 加速，请将 OpenSnell 服务端搭配 **Surge** 客户端，或任何支持 v5 的
客户端使用。

> **想要官方 Surge `snell-server` 没有的功能？**
> [`alpha`](https://github.com/missuo/opensnell/tree/alpha) 分支会跟随
> `main`，并在此基础上加入不属于 Surge 标准行为的实验扩展。目前包含
> `tcp-brutal` 拥塞控制。`main` 保持与官方服务端的严格互通性；
> `alpha` 用于承载 Surge 官方未提供的扩展功能。

### 为什么不支持 v1 / v2 / v3？

本项目有意不再支持早期 Snell 协议。它们的流帧格式早于 v4 的 padding/AEAD
重设计，如今在线路上已经很容易被指纹识别。尤其是 v1/v2/v3 的流量模式已经
无法可靠穿越 GFW，因此不建议用于新的部署。如果你仍有暂时不能下线的 v1/v2
旧环境，可以使用同类项目 [open-snell](https://github.com/icpz/open-snell)
及其分支；这些项目仍然实现了旧版本。本代码库只聚焦当前 Surge
`snell-server` 所使用的 v4/v5 线路协议。

### 那 Snell v6 呢？

Snell v6（`snell-server v6.0.0b1` / `v6.0.0b2` / `v6.0.0b3` / `v6.0.0b4`）已被**完整逆向并用 Go 重新实现**——
客户端与服务端都有——并且我们发出的帧在线路上与官方服务端**逐字节一致**（覆盖所有
swap-mode 与 write-mode 的 24 个 PSK 上，逐帧 padding 100% 匹配，且每个都能完整互通），
性能也与手写 C 的官方服务端持平。我们**选择不开源这套 v6 实现**。本仓库继续保持 v4/v5
（GPLv3）；如果现在就想跑 v6，[安装脚本](#安装)可以部署**官方** Surge
`snell-server v6.0.0b4`。完整说明见 [`SNELL_V6.md`](SNELL_V6.md)。

## 功能矩阵

| 路径                                  | `snell-server` | `snell-client` |
| ------------------------------------- | -------------- | -------------- |
| TCP CONNECT                           | ✅             | ✅             |
| 可复用的 TCP CONNECT（`CommandConnectV2`） | ✅        | ✅             |
| UDP-over-TCP（snell datagram）        | ✅             | ✅             |
| `http` / `tls` obfs                   | ✅             | ✅             |
| Dynamic Record Sizing（v5）           | ✅             | ✅             |
| `egress-interface`（v5）              | ✅             | —              |
| `ipv6` 出站地址族开关（v5）           | ✅             | —              |
| 自定义上游 DNS（`dns = …`）           | ✅             | —              |
| TCP Fast Open（仅 Linux）             | ✅             | ✅             |
| **QUIC 代理模式（v5）**               | ✅             | 使用 Surge     |

## 安装

### 一键安装服务端（Linux + systemd）

```sh
bash <(curl -fsSL https://s.ee/opensnell)
```

交互式安装器会：

- 让你选择 **OpenSnell**（默认，GPLv3，支持所有平台）、
  **官方 Surge `snell-server v5.0.1`** 或 **官方 Surge
  `snell-server v6.0.0b4`** beta（均为闭源，仅 Linux）。
- 选择 v6 时会写入新版 v6 配置（`dns-ip-preference` 取代 `ipv6`，
  `obfs` 已移除），客户端配置行输出 `version=6`。v6.0.0b4 是**静态链接**的，
  所以——不同于 b1 beta——不再需要安装任何额外的共享库。安装器还会询问
  **`mode`** 指令（见下方说明）。

> [!NOTE]
> **官方 `snell-server v6.0.0b4` 是闭源 beta。** Snell v6 引入了一层
> PSK 派生的逐帧**流量整形**（一段 padding keystream，外加 padding↔密文
> 交织，再叠加 AES-GCM），用于抗指纹。**b2 修好了让早期 b1 beta 不堪用的
> 两件事：** 现在是**静态链接**（无需额外共享库）且**多核**
> （`SO_REUSEPORT` + io_uring worker），不再把单核打满。在两台同机房主机间
> 实测，b2 以 **~10% 单核 CPU** 顶到 **~52 MB/s 链路天花板**，而 b1 只能
> **~30 MB/s 还烧满一个核**。**b3 新增 `mode` 指令：** `default`（obfs + AES，
> 行为不变）、`unshaped`（仅 AES、关闭整形——吞吐约 +10%，线路完全随机，等同
> v3）、`unsafe-raw`（明文、无加密——仅限可信隧道内）。客户端必须设置为**相同**
> 的 mode。**b4 是纯二进制更新——线路上与 b3 完全一致**（刷新依赖库、一处
> UDP 转发的内部修复，并移除了 UPX 自解压壳，因此现在以未压缩形式发布，约 2.85 MB，
> 仍为静态链接）：OpenSnell 无需任何改动即可与之互通，且版本号现在报告
> `v6.0.0b4`。它仍是 beta，所以追求最高稳定性时优先选 **OpenSnell** 或
> **Surge v5.0.1**；安装器会在安装 v6 前打印此说明。
- 如果 PSK 留空，使用 `openssl` 自动生成随机 PSK。
- 如果端口留空，在 `10000–60000` 范围内随机选择一个未占用端口。
- 写入 `/etc/snell/snell-server.conf`，安装 systemd unit
  （`snell-server.service`），在 UFW / firewalld 已启用时自动放行端口，
  并启动服务。
- 再次运行时可使用 `reconfigure`、`update`、`uninstall`、`start`、`stop`、
  `restart`、`status` 或 `info`；详见 `./install.sh help`。

### Docker / Docker Compose

服务端镜像发布在 GHCR：`ghcr.io/missuo/opensnell-server`
（多架构：`linux/amd64` 与 `linux/arm64`）。所有配置都通过环境变量传入，
入口脚本会在启动时将其渲染为 `snell-server.conf`。如果 `SNELL_PSK`
留空，容器首次启动时会自动生成随机 PSK 并打印到日志。

```yaml
# compose.yaml
services:
  snell-server:
    image: ghcr.io/missuo/opensnell-server:latest
    container_name: snell-server
    restart: unless-stopped
    ports:
      - "2333:2333/tcp"
      - "2333:2333/udp"
    environment:
      SNELL_LISTEN: "0.0.0.0:2333"
      SNELL_PSK: ""           # 留空则自动生成
      SNELL_OBFS: "off"
      SNELL_UDP: "true"
      SNELL_QUIC: "true"
      SNELL_IPV6: "true"
      SNELL_TFO: "false"
      # SNELL_EGRESS_INTERFACE: "eth0"
      # SNELL_DNS: "1.1.1.1, 8.8.8.8"
```

```sh
docker compose up -d
docker compose logs snell-server   # 留空 PSK 时，从日志里取自动生成的值
```

也可以直接用 `docker run`：

```sh
docker run -d --name snell-server --restart unless-stopped \
  -p 2333:2333/tcp -p 2333:2333/udp \
  -e SNELL_PSK=your-shared-secret \
  ghcr.io/missuo/opensnell-server:latest
```

带 tag 的版本可以使用 `:1.0.3`、`:1.0`、`:1` 等。

### 从源码构建

```sh
go build -o snell-server ./cmd/snell-server
go build -o snell-client ./cmd/snell-client
```

也可以直接安装：

```sh
go install github.com/missuo/opensnell/cmd/snell-server@latest
go install github.com/missuo/opensnell/cmd/snell-client@latest
```

## 服务端配置

`snell-server.conf` 通过 `-c <path>` 传入。所有配置项都位于
`[snell-server]` 段内。

```ini
[snell-server]

; 监听地址。必填。设置为 0.0.0.0:<port> 表示接受任意来源的连接；
; 如果前面还有反向代理，则可以设置为 127.0.0.1:<port>。
; 当 `quic = true`（默认）时，服务端还会在同一端口监听 UDP，
; 用于 QUIC 代理模式。因此，请确保主机前方的任何防火墙都同时放行
; TCP/<port> 和 UDP/<port>。
listen = 0.0.0.0:2333

; 预共享密钥。必填。它会按原始 UTF-8 字符串处理（不会进行 base64
; 解码），请确保这里的值与客户端配置完全一致。
psk = your-shared-secret

; 包裹 snell 流的混淆层。可选，默认关闭。
;   off  — 不启用混淆（推荐；v4/v5 帧格式已经通过逐帧 padding
;          对流量形态进行混淆）
;   http — 伪造 HTTP/1.1 Upgrade 握手
;   tls  — 伪造 TLS ClientHello/ServerHello 握手
obfs = off

; 是否接受客户端发来的 UDP-over-TCP（snell 自身的 datagram-in-stream
; 协议；与下方的 QUIC 模式不同）。可选，默认 true。
udp = true

; QUIC 代理模式（v5）。可选，默认 true。启用后，服务端会额外监听
; `listen` 中同一端口的 UDP，并接受包裹 QUIC Initial 数据包的
; snell 加密信封；一旦建立 `(src_ip, src_port) → upstream` 映射，
; 后续所有 UDP 数据包都会在两个方向上以原始 QUIC 形式转发。
; 如果要配合设置了 `block-quic=off` 的 Surge 客户端实现 HTTP/3
; 加速，必须启用该选项。
quic = true

; 绑定出站网卡。可选，默认留空（使用主机默认路由）。设置后，所有
; 上游 socket，包括 TCP 拨号、UDP-over-TCP 监听器以及 QUIC 上游拨号，
; 都会绑定到该网卡：Linux 使用 SO_BINDTODEVICE，macOS 使用 IP_BOUND_IF。
; 其他平台会在拨号时拒绝该配置。
egress-interface =

; 出站拨号是否可以使用 IPv6 目标地址。可选，默认 true，与官方 Surge
; snell-server 的 `ipv6 = true` 一致。设置为 false 时，拨号器会被限制为
; "tcp4" / "udp4"；Go 解析器只会考虑 A 记录，并跳过 AAAA 查询。
; 适用于 IPv6 路径不可用或较慢的主机。该选项只影响出站连接；
; 服务端监听哪些地址仍由 `listen` 控制（如需 v6 双栈入站，请写
; `[::]:2333`）。
ipv6 = true

; 上游 DNS 服务器列表，逗号分隔。可选，默认留空（走 /etc/resolv.conf
; 的系统解析器）。用于解析客户端请求里的目标域名。每一项是 v4 或 v6
; 的 IP 字面量，可带 `:port` 后缀；不写端口时默认 53。多个服务器按顺序
; 重试。对应官方 Surge snell-server 在 v4.1.0 加的 `dns = …` 选项。
; 启动时每个生效的服务器会输出一行
;   level=INFO msg="effective DNS" server=<addr>
dns =

; TCP Fast Open（RFC 7413）。可选，默认 false。启用后，入站 TCP
; 监听器和出站上游 TCP 拨号都会设置 TFO，让 snell 客户端第一次写入的
; 数据可以随 SYN 包一起发送，从而为每条新 TCP 连接节省一个 RTT。
; 仅 Linux 支持（使用 TCP_FASTOPEN / TCP_FASTOPEN_CONNECT）。
; 需要内核 sysctl `net.ipv4.tcp_fastopen` 打开服务端所需的 bit 1
; （可运行 `sysctl -w net.ipv4.tcp_fastopen=3`）。其他平台上该选项
; 会被静默忽略。
tfo = false
```

运行：

```sh
./snell-server -c snell-server.conf       # info 级别日志
./snell-server -c snell-server.conf -v    # debug 级别日志
```

一个最小的 systemd unit 可以写成：

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

## 客户端配置

`snell-client.conf` 会暴露一个本地 **SOCKS5** 代理（TCP CONNECT 加
UDP ASSOCIATE），并把每个接收到的请求都通过 snell 服务端建立隧道。
如需使用 QUIC/HTTP-3，请使用 Surge 作为前端；本客户端面向已经支持
SOCKS5 的工具，例如 `curl --socks5-hostname`、浏览器代理设置、
应用内 SOCKS5 接口等。

```ini
[snell-client]

; 本地 SOCKS5 监听地址。必填。除非确实要把代理暴露到局域网，
; 否则请绑定到 127.0.0.1。
listen = 127.0.0.1:1080

; 远端 snell 服务端，格式为 host:port。必填。
server = your-server.example.com:2333

; 预共享密钥，必须与服务端的 `psk` 逐字节一致。
psk = your-shared-secret

; 此客户端声明的 Snell 协议版本。可选，默认 v5。
;   v4 — 明确声明为 v4 客户端
;   v5 — 明确声明为 v5 客户端（推荐）
; v4 与 v5 使用相同的 TCP 线路格式，因此该字段目前只提供信息
; （启动时会写入日志）。Surge v5 服务端文档说明它向后兼容 v4 客户端。
version = v5

; 混淆层。可选，默认关闭。必须与服务端设置一致。有效值：off | http | tls。
obfs = off

; http/tls 混淆层使用的 Host header / SNI。可选，默认复用 server 主机名。
obfs-host = bing.com

; 是否在多个 SOCKS5 请求之间复用上游 TCP 连接
; （snell 的 `CommandConnectV2`）。可选，默认 false。短 HTTP 请求建议启用；
; 连接池会把每条 TCP 连接限制为最多 2 个会话，以匹配真实 Surge 服务端的
; 行为；连接放回池之前还会排空服务端半关闭产生的 zero chunk，
; 确保下一次复用从干净的帧边界开始。
reuse = true

; 连接 snell 服务端时，在出站拨号上启用 TCP Fast Open。可选，默认 false。
; 仅 Linux 支持；内核 sysctl 要求见上方服务端 `tfo` 说明。
tfo = false
```

运行：

```sh
./snell-client -c snell-client.conf       # info 级别日志
./snell-client -c snell-client.conf -v    # debug 级别日志
```

### 端到端冒烟测试示例

```sh
# 另开一个终端，并确保 snell-client 正在 127.0.0.1:1080 上运行：
curl -sS --socks5-hostname 127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace
# 预期响应正文中的 `ip=` 行会显示 snell-server 的出口 IP。
```

## 将 OpenSnell 服务端与 Surge 配合使用（推荐用于 QUIC/HTTP-3）

在 Surge 配置中，将该服务端添加为 snell 代理，设置 `version=5`，
并关闭 Surge 针对每条连接的 QUIC 阻断：

```ini
[Proxy]
my-snell = snell, your-server.example.com, 2333, psk=your-shared-secret, version=5, tfo=true, block-quic=off
```

当 Surge 通过 `my-snell` 分发 HTTP/3 连接时，它会把最初 1 到 2 个
QUIC Initial 数据包包裹在 snell 信封里。该信封包含目标 SNI/host，
因此这些信息不会在线路上明文暴露。之后的其余数据包会以原始 QUIC
形式转发，由 `snell-server` 的 `ServeQUIC` 循环处理。

## 协议说明

### TCP 帧布局（v4 / v5）

- **密钥派生**：`argon2id(psk_utf8, salt, t=3, m=8 KiB, p=1)` →
  32 字节；前 16 字节作为 AES-128-GCM 密钥。
- 每个方向都有一个 16 字节随机 salt，在第一帧之前发送一次。
- 每帧包含一个 7 字节明文头
  `[type=4, 0, 0, padLen_be, payloadLen_be]`，该头会被 AEAD 密封
  （nonce=N），随后是 `padLen` 字节 padding，以及被 AEAD 密封的 payload
  （nonce=N+1）。nonce 计数器是一个 12 字节小端递增值。
- padding 区域中偶数索引处的字节会与 payload 密文开头的字节交换
  （见 `swapPadding`），因此原始 padding 字节不会在线路上连续出现。
- 每条流的第一帧都会额外携带一段 `0x100..0x1FF` 字节的 padding，
  其长度会被选择到让 salt+padding+ciphertext 的整体 0/1 比例落在
  “自然”的范围内；后续帧会将最大 payload 从较小的初始值逐步提高到
  `MaxPayloadLength`，并在空闲 30 秒后重置。这就是 v5 的
  **Dynamic Record Sizing** 优化。

### QUIC 信封布局（仅 v5，客户端 → 服务端，一条流的第一个数据包）

```
[salt(16B random)]
[AEAD-Seal(K, nonce=0, [0x04, 0, 0, padLen_be, payloadLen_be]) || 16B tag]
[padding(padLen)]
[AEAD-Seal(K, nonce=1, request_header || inner_QUIC_packet) || 16B tag]

request_header = [0x01, 0x01, 0x00, hostlen, host, port_be]
K              = Argon2id(psk_utf8, salt, 3, 8 KiB, 1, 32)[:16]
AEAD           = AES-128-GCM
```

服务端解码第一个信封并记录 `(client_src, upstream)` 映射后，两个方向上的
后续 UDP 数据包都会以**原始 QUIC**形式转发，不再附加任何 snell 帧封装。

该格式是通过抓取 Surge 客户端产生的真实 HTTP/3 流量，并使用配置中的 PSK
解密后，与官方 Surge `snell-server v5.0.1` 对照逆向得到的。详见
`components/snell/quic.go` 和 `components/snell/quic_test.go`；单元测试中
包含一个真实抓取到的 1359 字节信封作为 fixture。

## 性能

我们在两台同机房 Linux 主机上，将 OpenSnell 与官方 `snell-server v5.0.1`
（也就是 Surge 客户端背后的那份闭源二进制）做了基准对比：其中一台主机在
不同端口上**同时**运行两个服务端，让两边共用同一条上游链路、同一套内核和
同一份 CDN 冷却状态；另一台主机运行两个 snell-client 实例，分别指向这两个
服务端。所有流量都通过 SOCKS5，经 `curl --socks5-hostname` 访问同一个上游 URL。

### 测试方法

测试分三组依次进行（从不同时运行）。每个被测对象之间都会停顿几秒，避免上游
CDN 对其中一方限速：

1. **延迟** — 对一个极小端点连续请求 50 次
   （`cloudflare.com/cdn-cgi/trace`，响应约 200 B）。通过 `curl -w`
   测量 `time_connect`、TTFB 和总耗时。
2. **并发吞吐** — 以 N = 2、4、8 路并行下载一个 10 MB 文件。聚合 MB/s =
   总字节数 ÷ 墙钟时间。
3. **抓包分析** — 每个变体各下载一次 10 MB 文件，同时在服务端侧运行
   `tcpdump`，统计满载 TCP segment 与空 ACK 的数量。

### 官方二进制实际是什么

我们反汇编了官方 `snell-server-v5.0.1-linux-amd64`（1.2 MB，静态链接，
section headers 已剥离）。字符串分析显示它由 **GCC** 构建，链接了
**libuv**（curl 与 Node.js 使用的同一套异步 I/O 库），并使用 **OpenSSL**
的 AES-NI GCM 实现（其中可以看到特征字符串 `GCM module for x86_64`）。
简而言之，它是 **C/C++ + libuv + OpenSSL**。这一点很重要，因为 libuv
会把整个代理运行在单个 event-loop 线程上：没有按连接分配的 goroutine，
没有 GMP 调度，也没有 GC。

### 初次结果（OpenSnell v1.0.1）

| 指标                                         | OpenSnell v1.0.1 | 官方 v5.0.1   | Δ           |
| -------------------------------------------- | ---------------: | ------------: | ----------- |
| TTFB 中位数                                  |       噪声范围内 |    噪声范围内 | ~0          |
| 单流吞吐                                    |             持平 |          持平 | ~0          |
| **N = 8 并发吞吐**                           |    **6.49 MB/s** | **8.46 MB/s** | **−30 %**   |
| 一次 10 MB 传输中的空 ACK 数                 |             1444 |          1084 | **+33 %**   |

单流吞吐和延迟已经与官方服务端基本一致，差距主要集中在并发吞吐上。

### 根因

`v4Reader.readFrame()` 反序列化每个 snell 帧时会进行**两次独立的
`io.ReadFull` 调用**：一次读取 23 字节的 AEAD 帧头，一次读取 padding +
payload + tag。而底层 `net.Conn` 当时是直接读取，没有用户态缓冲。按典型
帧大小约 1.5 KB 计算，一次 10 MB 传输会经过约 7300 个帧，因此每个方向需要
大约 **14000 次 `recv()` 系统调用**。

由此带来两个结果：

1. **空 ACK 增多。** Linux 在应用层大块排空接收缓冲区时会延迟 ACK，
   但如果应用层不断进行小读，内核就会更积极地发送 ACK。每帧两个 syscall
   意味着大量小读，也就破坏了 delayed-ACK，导致线路上的空 ACK 比 C 参考实现
   多约 33%。
2. **并发吞吐下降。** 每条 snell 连接会运行两个 goroutine（每个方向一个）。
   N = 8 个并发 SOCKS5 会话意味着 16 个 goroutine，它们都在执行大量小 syscall，
   并通过 Go runtime 调度切换。libuv 没有这部分开销；它的单个 epoll 驱动线程
   可以以满速吸收新的 TCP 数据。

### 修复

只改一行：

```go
// components/snell/v4.go — initReader()
c.r = &v4Reader{Reader: bufio.NewReaderSize(c.Conn, 64*1024), aead: aead}
```

64 KB 的读缓冲可以让一次 `recv()` 把约 40 个最大尺寸的 snell 帧拉入用户态，
使读取路径上的系统调用数量大约减少 **90 倍**。这个改动对线路格式完全透明：
v4 帧解析器看到的仍是同一条字节流，只是这些字节通过更少的系统调用送达。

### OpenSnell v1.0.2 之后

| 指标                                         | OpenSnell v1.0.2 | 官方 v5.0.1    | Δ           |
| -------------------------------------------- | ---------------: | -------------: | ----------- |
| TTFB 中位数                                  |          17.9 ms |        17.1 ms | +4.7 %      |
| TTFB p95                                     |          25.4 ms |        24.5 ms | +3.7 %      |
| N = 2 吞吐                                   |      43.48 MB/s  |    44.44 MB/s  | −2.2 %      |
| **N = 8 吞吐**                               |   **47.34 MB/s** | **48.19 MB/s** | **−1.8 %**  |
| 一次 10 MB 传输中的空 ACK 数                 |             2596 |           2343 | **+10.8 %** |

并发吞吐差距从 **−30 %** 收敛到 **−1.8 %**，空 ACK 超出量也从 **+33 %**
降到 **+10.8 %**。剩余约 11% 的 ACK 差异和约 2% 的吞吐差异，很可能来自
Go runtime 相对手写 C event loop 的额外开销，并且已经低于真实工作负载中的
可感知噪声。

### 结论

在 Surge 已公开的 snell v5 线路协议上，OpenSnell 的 `snell-server` 在并发
场景下可以达到官方 C 参考实现**约 98% 的性能**，延迟表现则**几乎不可区分**。
这次 bufio 修复在 `components/snell/v4.go` 中只有 `+9/−1` 行；它也说明，
缩小与原生 C/libuv 实现之间的差距时，最值得剖析的往往是读取路径，而不只是
应用层逻辑。

## 与真实 Surge `snell-server` 的互通性

已针对 `snell-server v5.0.1 (Nov 19 2025)` 完成测试：

| 路径                                      | 结果                                             |
| ----------------------------------------- | ------------------------------------------------ |
| 我方客户端 → 真实服务端，TCP              | ✅ 10/10                                         |
| 我方客户端 → 真实服务端，UDP-over-TCP     | ✅ DNS 往返成功                                  |
| 我方客户端 → 真实服务端，复用             | ✅ 30 次串行 + 20 次并发                         |
| 我方服务端，QUIC 模式，真实 Surge 信封    | ✅ 基于真实抓包的单元测试通过                    |
| HTTP/3 → 我方服务端 → Cloudflare          | ✅ 5/5（`ip=` 回显我方服务端，`sni=plaintext`） |

## 参考资料

- [MetaCubeX/mihomo#2816](https://github.com/MetaCubeX/mihomo/pull/2816) —
  较早的 Snell v5 逆向提案，后来因 #2817 而关闭；其中对 AEAD 帧布局和
  padding 交错算法的描述，是本实现的起点。
- [MetaCubeX/mihomo#2817](https://github.com/MetaCubeX/mihomo/pull/2817) —
  mihomo 合并的 Snell v4/v5 outbound + inbound 实现；本项目的 TCP 协议层
  移植自该实现，并改造为独立的服务端/客户端，同时移除了 v1/v2/v3 支持。
- [Surge snell release notes](https://kb.nssurge.com/surge-knowledge-base/release-notes/snell) —
  上游按版本发布的功能列表。

## 许可证

GPLv3 — 见 [LICENSE.md](LICENSE.md)。obfs、socks5 和 buffer-pool 的部分代码
来自 open-snell / clash 项目。
