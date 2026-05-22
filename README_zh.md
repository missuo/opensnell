# OpenSnell (alpha 分支)

[English](README.md) | 简体中文

> **你看到的是 `alpha` 分支 README**。这个分支跟踪
> [`main`](https://github.com/missuo/opensnell/tree/main)，并在其上叠加
> 一些**官方 Surge `snell-server` 并不提供**的实验性功能 —— 目前包括
> `tcp-brutal` 拥塞控制。如果只需要与 Surge 官方完全互操作的行为，请用
> `main` 分支及其 tag release；只有当你明确需要本分支文档里列出的额外
> 功能时，才用 `alpha`。

OpenSnell 是 Snell 代理协议 **v4** 和 **v5** 的 Go 实现，包含服务端与
客户端。下文列出的所有路径都已经过验证，可与官方 Surge `snell-server v5.0.1`
端到端互通。

Snell v5 的 UDP/QUIC 代理模式目前只在**服务端**实现；如果要为下游应用启用
HTTP/3 加速，请将 OpenSnell 服务端搭配 **Surge** 客户端，或任何支持 v5 的
客户端使用。

### 为什么不支持 v1 / v2 / v3？

本项目有意不再支持早期 Snell 协议。它们的流帧格式早于 v4 的 padding/AEAD
重设计，如今在线路上已经很容易被指纹识别。尤其是 v1/v2/v3 的流量模式已经
无法可靠穿越 GFW，因此不建议用于新的部署。如果你仍有暂时不能下线的 v1/v2
旧环境，可以使用同类项目 [open-snell](https://github.com/icpz/open-snell)
及其分支；这些项目仍然实现了旧版本。本代码库只聚焦当前 Surge
`snell-server` 所使用的 v4/v5 线路协议。

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
| **`tcp-brutal` 拥塞控制（实验性，仅 Linux）** | ✅     | ✅             |
| **TUN 入站（实验性，仅 Linux）**      | —              | ✅             |

## 安装

### 一键安装服务端（Linux + systemd）

```sh
bash <(curl -fsSL https://s.ee/opensnell)
```

交互式安装器会：

- 让你选择 **OpenSnell**（默认，GPLv3，支持所有平台）或
  **官方 Surge `snell-server v5.0.1`**（闭源，仅 Linux）。
- 如果 PSK 留空，使用 `openssl` 自动生成随机 PSK。
- 如果端口留空，在 `10000–60000` 范围内随机选择一个未占用端口。
- 写入 `/etc/snell/snell-server.conf`，安装 systemd unit
  （`snell-server.service`），在 UFW / firewalld 已启用时自动放行端口，
  并启动服务。
- 再次运行时可使用 `reconfigure`、`update`、`uninstall`、`start`、`stop`、
  `restart`、`status` 或 `info`；详见 `./install.sh help`。

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

; --- 实验性：tcp-brutal 拥塞控制 ---
; 启用后，每条从 snell 客户端接进来的 TCP 连接都会切换到
; apernet/tcp-brutal 的内核 CC 算法，并按 `brutal-mbps` 固定发送速率。
; 仅支持 Linux；需要先 `apt install linux-headers-$(uname -r)`，再
; 从 https://github.com/apernet/tcp-brutal clone 编译并 `insmod brutal.ko`
; 加载模块。若内核模块未加载，本服务端会打 warning 然后退回默认 CC，
; 不会因此拒绝连接。
;
; ⚠️ 警告：brutal 的速率是**每条 TCP 连接独立**生效的。Snell 没有原生
; 多路复用，多个并发 SOCKS5 会话各自吃满 brutal-mbps，会把接收端撑爆。
; 只在以下场景启用：单流长任务（大文件下载/视频流）、或配合
; `reuse = true` 且明确低并发的工作负载，或纯测试用途。
brutal = false
brutal-mbps = 100         ; brutal=true 时必填；每条连接的速率（Mbit/s）
brutal-cwnd-gain = 15     ; 可选；以十分之一为单位（15 = 1.5x，20 = 2.0x）
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

; --- 实验性：客户端出向的 tcp-brutal 拥塞控制 ---
; 启用后，每条到 snell 服务端的 TCP 出向连接都会切换到 brutal CC。
; 仅 Linux。客户端这一侧只影响 client→server 方向（上传）；
; 要让下载方向也走 brutal，需要在**服务端**也开同样的选项。
; 同样存在多路复用约束（见服务端配置中的警告）。默认关闭。
brutal = false
brutal-mbps = 100         ; brutal=true 时必填；每条连接的上传速率
brutal-cwnd-gain = 15     ; 可选；以十分之一为单位
```

运行：

```sh
./snell-client -c snell-client.conf       # info 级别日志
./snell-client -c snell-client.conf -v    # debug 级别日志
```

### 实验性：TUN 入站 — 透明 TCP 接管（仅 Linux）

除了 SOCKS5 监听之外，`snell-client` 还可以**透明地接管机器上所有
新发起的出向 TCP 连接**并通过 snell 服务器转发——应用本身完全不需要
懂 SOCKS5。

底层是 **nftables REDIRECT**（sing-tun 的 `auto-redirect` 模式），
而**不是**默认路由劫持。这两种方案的区别很关键：默认路由劫持的方式会
顺带破坏本机自身对外的服务，因为 Linux FIB 层无法在「本机服务对外回包」
和「本机进程主动发起的新出向」之间做出区分。让 nftables + conntrack
来做这个分类，分类语义就自然正确：

| 流量                                                | 行为 |
| --------------------------------------------------- | ---- |
| 进站 TCP/UDP 到本机服务（sshd / nginx / caddy / realm…） | **不动**——`PREROUTING` 把目标为本机的 IP 全部放行 |
| 上述服务对外的回包                                   | **不动**——`ct mark` 在 conntrack 上跳过 redirect |
| 本机进程发起的新出向 TCP                             | **重定向** → snell 服务器 |
| `snell-client` 自己连向 snell 服务器的 TCP           | **不动**——`SO_MARK` 0x2024 在 OUTPUT 链命中并提前返回 |
| 本机进程的 UDP / ICMP                                | **不动**——v1 只接管 TCP |
| `FORWARD` 链路由转发（容器、`ip_forward=1`）         | **不动**——我们不动 FORWARD 链 |

这意味着：任意来源 IP 发起的新 SSH 都能连进来；本机已有的 TCP 服务
不会被影响；`snell-client` 进程退出或重启之后机器回到与开启 TUN 前
完全一致的状态（nftables 表和那条优先级 1 的 `ip rule` 在
`SIGTERM` / `SIGINT` 时由 `Inbound.Close()` 完整反卷）。

环境要求：

- **仅 Linux**，内核加载 `nf_tables` + `nf_conntrack` 模块（Debian 12+
  / RHEL 9+ 默认满足）。
- **`nft` 命令在 `$PATH` 里**。
- **需要 `CAP_NET_ADMIN`**（实际操作就是以 root 运行，或通过 systemd
  `AmbientCapabilities` 授权）。**只用 SOCKS5 模式时仍然不需要 root**，
  同一份二进制两种用法都行。

让特定的服务（`realm` / `gost` / `socat` 这类端口转发器）从重定向中
旁路出去——这些工具的出向语义是「透明转发到远端目标」，你通常希望它们
继续走本机自然出口，而**不是**被改道到 snell 服务器：

- Linux 的 nftables 在内核层面只能按 **UID** 或 **GID** 来匹配 socket
  的归属。`PID` 不稳定不能用，**进程名 / `comm` 在 netfilter 层根本
  不暴露**——这是 Linux 内核的硬限制，对 clash / sing-box / v2ray-redir
  / ss-redir 等所有透明代理都一样。
- 标准做法是让转发器进程以独立用户运行。对 systemd 管理的服务，
  在 unit 文件里加 `User=realm`（先 `useradd -r realm` 创建一个系统
  用户）。
- 然后在下面的 `exclude-uid` 里把对应的用户名/UID 列出来。

在原有 `[snell-client]` 之外加一个 `[snell-tun]` 段：

```ini
[snell-tun]

; 总开关。默认 false — 不写或为 false 时 snell-client 行为与历史一致
; （SOCKS5-only）。也可以用命令行 --tun 强制开启。
enable = true

; 逗号分隔的 UID 列表或用户名列表，这些用户的出向 TCP 将旁路 redirect、
; 走机器原本的默认网关直连。用户名在启动时通过系统 passwd 数据库解析。
;
; 常见用例：透明转发类服务（每个跑在自己专用的 user 下）。下面这条
; 配置让 realm、gost 两类服务保持以本机出口 IP 直连远端目标。
exclude-uid = realm, gost
```

启动：

```sh
sudo ./snell-client --tun -c snell-client.conf
```

可以保留 `[snell-client]` 的 `listen = 127.0.0.1:1080` 让 SOCKS5 与
TUN 同时运行，也可以把它改成 `listen = off` 跑纯 TUN 模式。

v1 暂未实现（已列入后续计划）：UDP 流量接管（需要真正的 TUN 设备 +
用户态 IP 协议栈）；cgroup-v2 维度的排除（让 systemd 单元不用改
`User=` 也能精准旁路）；fake-IP DNS 让线路上以 `AtypDomainName` 编码
保留域名信息；macOS 与 Windows。

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

## 实验性：tcp-brutal 拥塞控制

[apernet/tcp-brutal](https://github.com/apernet/tcp-brutal) 是一个 Linux
内核模块，向系统注册了一个名为 `brutal` 的 TCP 拥塞控制算法。用户态
对单个 socket 设置一个固定发送速率,内核就按这个速率发包,不再听
ACK 反馈。在高丢包的"长肥管道"上,cubic/bbr 都收不住时,brutal 能直接
按配置打满。

OpenSnell 怎么用:

```sh
# 服务端宿主机(Linux):
apt install linux-headers-$(uname -r) make gcc
git clone https://github.com/apernet/tcp-brutal /tmp/tcp-brutal
cd /tmp/tcp-brutal && make && insmod brutal.ko
# 验证:
cat /proc/sys/net/ipv4/tcp_available_congestion_control   # 应该出现 "brutal"
```

然后在 `snell-server.conf`(以及/或 `snell-client.conf`)里设
`brutal = true`、`brutal-mbps = <Mbps>`。重启服务后,**该方向**的
每条 TCP 出向都会被精确限速。

**端到端实测**(v6-capable Linux server,`brutal-mbps = 50`,100 MB
下载来自 `cachefly.cachefly.net/100mb.test`):

| 服务端 CC      | 耗时    | 速率       |
| -------------- | ------- | ---------- |
| vanilla(cubic)| 1.89 s  | 444 Mbps   |
| brutal(50)    | 16.97 s | **49.4 Mbps**(误差 <2%)|

### ⚠️ 开启之前必须读

引上游 maintainers 的原话:

> "Brutal 的速率是**逐连接**(per individual connection)生效的。这让它
> 只适合带多路复用(mux)的协议。对每条代理连接都要新开一条 TCP 的
> 协议来说,多个并发连接同时活跃时,brutal 会**把接收端撑爆**。"

Snell **没有 mux**。reuse 模式(`CommandConnectV2`)是**串行**复用——
同一时刻一条 TCP 上只有一个 CONNECT 会话——但 client 仍然可能并行持
有好几条 pooled TCP。**如果你的工作负载同时打开 N 条 snell 连接,
服务端会试图按 N × `brutal-mbps` 总速率推**,接收端会丢包到崩。
只在下面这些情况开:

- 单流长任务(大文件下载、视频流)
- 配合 `reuse = true` + 已知并发很低
- 测试

两端可以独立启用。**只在服务端开** → 控制 server → client(下载,
也是代理的主要方向);**只在客户端开** → 控制 client → server(上传)。
两端配置不需要相同。

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
