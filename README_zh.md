# OpenSnell（alpha 分支）

[English](README.md) | 简体中文

> **你正在阅读 `alpha` 分支的 README。** 这个分支跟踪
> [`main`](https://github.com/missuo/opensnell/tree/main)，并在其上加入
> 官方 Surge `snell-server` **不包含**的实验性功能。目前这里指的是
> `tcp-brutal` 拥塞控制。如果你只需要与 Surge 兼容的标准行为，请使用
> `main` 分支及其带标签的正式发布版本。只有在明确需要本文档列出的额外功能时，
> 才建议使用 `alpha` 分支。

OpenSnell 是 Snell 代理协议 **v4** 和 **v5** 的 Go 实现，包含服务端和客户端。
下文列出的每条路径都已与官方 Surge `snell-server v5.0.1` 完成端到端互通验证。

Snell v5 的 UDP/QUIC 代理模式目前仅在**服务端**支持；如果需要为下游应用启用
HTTP/3 加速，请将 OpenSnell 服务端搭配 **Surge** 客户端，或其他支持 v5 的客户端使用。

### 为什么不支持 v1 / v2 / v3？

本项目有意不再支持更早的 Snell 协议版本。它们的流帧格式早于 v4 的 padding/AEAD
重设计，如今在线路上已经很容易被指纹识别。尤其是 v1/v2/v3 的流量特征已经无法
可靠穿越 GFW，因此不推荐用于新的部署。如果你还有暂时无法下线的 v1/v2 旧环境，
同类项目 [open-snell](https://github.com/icpz/open-snell) 及其分支仍然实现了这些版本；
本代码库只聚焦当前 Surge `snell-server` 所使用的 v4/v5 线路协议。

## 功能矩阵

| 路径                                  | `snell-server` | `snell-client` |
| ------------------------------------- | -------------- | -------------- |
| TCP CONNECT                           | ✅             | ✅             |
| 可复用 TCP CONNECT（`CommandConnectV2`） | ✅          | ✅             |
| UDP-over-TCP（snell datagram）        | ✅             | ✅             |
| `http` / `tls` obfs                   | ✅             | ✅             |
| Dynamic Record Sizing（v5）           | ✅             | ✅             |
| `egress-interface`（v5）              | ✅             | —              |
| `ipv6` 出站地址族开关（v5）           | ✅             | —              |
| 自定义上游 DNS（`dns = …`）           | ✅             | —              |
| TCP Fast Open（仅 Linux）             | ✅             | ✅             |
| **QUIC 代理模式（v5）**               | ✅             | 使用 Surge     |
| **`tcp-brutal` 拥塞控制（实验性，仅 Linux）** | ✅     | ✅             |
| **TUN 入站 + fake-IP DNS（实验性，Linux + macOS）** | — | ✅       |
| **TUN：Direct IP / Direct Domain 旁路（实验性）** | — | ✅                  |
| **TUN：通过隧道探测 IPv6 可达性** | — | ✅                                    |
| **TUN：UDP/443 注入 ICMP Unreachable 触发 QUIC 快速回退** | — | ✅          |

## 安装

### 一键安装服务端（Linux + systemd）

```sh
bash <(curl -fsSL https://s.ee/opensnell)
```

交互式安装器会：

- 让你在 **OpenSnell**（默认，GPLv3，跨平台）和
  **官方 Surge `snell-server v5.0.1`**（闭源，仅 Linux）之间选择。
- 如果 PSK 留空，会使用 `openssl` 生成随机 PSK。
- 如果端口留空，会在 `10000–60000` 范围内选择一个未占用的随机端口。
- 写入 `/etc/snell/snell-server.conf`，安装 systemd unit
  （`snell-server.service`），在 UFW / firewalld 已启用时自动放行端口，
  并启动服务。
- 再次运行时可以使用 `reconfigure`、`update`、`uninstall`、`start`、
  `stop`、`restart`、`status` 或 `info`；详见 `./install.sh help`。

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
; 如果前面还有其他反向代理，则可以设置为 127.0.0.1:<port>。
; 当 `quic = true`（默认）时，服务端还会在同一端口监听 UDP，
; 用于 QUIC 代理模式。因此，请确保主机前方的防火墙同时放行
; TCP/<port> 和 UDP/<port>。
listen = 0.0.0.0:2333

; 预共享密钥。必填。按原始 UTF-8 字符串处理（不会进行 base64 解码），
; 请确保这里的值与客户端配置完全一致。
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

; QUIC 代理模式（v5）。可选，默认 true。启用后，服务端还会监听
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
; 适用于 IPv6 路径不可用或速度较慢的主机。该选项只影响出站连接；
; 服务端监听哪些地址仍由 `listen` 控制（如需 v6 双栈入站，请写
; `[::]:2333`）。
ipv6 = true

; 上游 DNS 服务器列表，逗号分隔。可选，默认留空（使用 /etc/resolv.conf
; 中的系统解析器）。用于解析客户端请求里的目标域名。每一项都是 v4
; 或 v6 的 IP 字面量，可以带 `:port` 后缀；不写端口时默认 53。
; 多个服务器会在每次查询时按顺序尝试。对应官方 Surge snell-server
; 在 v4.1.0 加入的 `dns = …` 指令。启动时每个生效服务器都会输出：
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
; 启用后，每条从 snell 客户端接入的 TCP 连接都会切换到
; apernet/tcp-brutal 内核拥塞控制算法，并按 `brutal-mbps` 固定发送速率。
; 仅 Linux 支持；需要先执行 `apt install linux-headers-$(uname -r) && cd
; tcp-brutal && make && make load`。如果内核模块缺失，服务端只会记录 warning
; 并回退到默认拥塞控制。
;
; ⚠️  警告：brutal 的速率是按**每条 TCP 连接**独立生效的。Snell 没有原生
; mux，因此多个并发 SOCKS5 会话会各自获得完整速率，可能压垮接收端。
; 只有在单流工作负载，或配合 `reuse = true` 且确认并发很低时，才适合启用。
brutal = false
brutal-mbps = 100         ; brutal = true 时必填；每条连接的速率（Mbit/s）
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
如需使用 QUIC/HTTP-3，请使用 Surge 作为前端；本客户端面向已经支持 SOCKS5
的工具，例如 `curl --socks5-hostname`、浏览器代理设置、应用内 SOCKS5 接口等。

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
; 启用后，每条连接到 snell 服务端的 TCP 出向连接都会切换到 brutal CC。
; 仅 Linux 支持；这里只控制 CLIENT→SERVER 方向（上传）。如需下载方向加速，
; 需要在服务端启用同样的选项。多路复用方面的限制与服务端选项相同。
; 默认关闭。
brutal = false
brutal-mbps = 100         ; brutal = true 时必填；每条连接的上传速率
brutal-cwnd-gain = 15     ; 可选；以十分之一为单位
```

运行：

```sh
./snell-client -c snell-client.conf       # info 级别日志
./snell-client -c snell-client.conf -v    # debug 级别日志
```

### 实验性：TUN 入站 — 透明 TCP 接管 + fake-IP DNS（Linux + macOS）

除了 SOCKS5 监听之外，`snell-client` 还可以透明接管机器上**每条新发起的出站
TCP 连接**，并通过 snell 服务器转发。应用本身不需要了解 SOCKS5，同时**原始
主机名会端到端保留**，由 snell 服务器自行完成干净的 DNS 解析。整体架构与
clash / mihomo / sing-box 使用的思路一致。

#### 架构

两个平台用于 DNS 拦截和出站 TCP 接管的内核 hook 不同，但最终都会汇入同一套
用户态 fake-IP 流水线：

```
   ┌──────────────── Linux ─────────────────┐  ┌──────────────── macOS ────────────────┐
   │ nftables DNAT UDP/TCP :53              │  │ 以程序方式改写系统 DNS：             │
   │   → 198.18.128.1:53  (TUN gateway)     │  │  networksetup -setdnsservers <svc>    │
   │                                        │  │   198.18.128.1   (退出时恢复)        │
   └────────────────────┬───────────────────┘  └────────────────────┬──────────────────┘
                        ▼                                           ▼
                  ┌─────────────────────────────────────────────────┐
                  │  进程内 fake-IP DNS server                     │
                  │  A    → 为主机名分配 198.18.128.N 并返回；     │
                  │         pool 是 LRU，且支持双向查找            │
                  │  AAAA → 空 NOERROR（应用回退到 A）             │
                  │  其他类型 → SERVFAIL（不转发）                 │
                  └────────────────────┬────────────────────────────┘
                                       ▼
   ┌──────────────── Linux ─────────────────┐  ┌──────────────── macOS ────────────────┐
   │ 出站 TCP 接管：                        │  │ 出站 TCP 接管：                       │
   │   TUN 上的 sing-tun "system" stack     │  │   TUN auto-route，使用 8 条           │
   │   处理 fake-IP TCP；                   │  │   sub-prefix 路由                     │
   │   sing-tun AutoRedirect（nftables      │  │   (1/8, 2/7, 4/6, 8/5, 16/4, 32/3,    │
   │   REDIRECT）处理 real-IP TCP           │  │   64/2, 128/1) 覆盖整个 v4，          │
   │                                        │  │   排除 snell 服务器 IP                │
   └────────────────────┬───────────────────┘  └────────────────────┬──────────────────┘
                        ▼                                           ▼
                  ┌─────────────────────────────────────────────────┐
                  │  共享 handler：                                │
                  │   dst ∈ fake-IP 前缀？→ 反查 pool              │
                  │                         → DialTCP(name, port)  │
                  │                           使用 AtypDomainName  │
                  │   否则                 → DialTCP(ip, port)     │
                  │                           使用 AtypIPv4        │
                  └────────────────────┬────────────────────────────┘
                                       ▼
                          snell 服务器（使用自己的解析器）
                                       │
                                       ▼
                          真实目标，干净 DNS
```

fake-IP 层之所以重要，是因为很多用户所在网络的 ISP DNS 会**污染** docker.io、
github.com、googlevideo.com 等域名。如果本机先完成解析，只有一个 IP 被送到 snell
服务器，那么这个 IP 可能已经是错的，snell 也无能为力。使用 fake-IP 后，snell
服务器会从它自己认为干净的上游解析目标；你也可以通过服务端的 `dns = …` 固定这个上游。

#### IPv6 策略

fake-IP 路径**仅 IPv4**：AAAA 查询返回空 NOERROR，让解析器回退到 A；
snell 服务器随后用自己的 DNS 解析主机名并以 v4 建连。

仅此一项不足以处理那些**绕过了我们 fake-DNS** 的流量 —— 例如 Chrome 的
Secure DNS / Firefox DoH、缓存了 AAAA 的服务、直接连 v6 字面量的应用。
对于这类情况，TUN handler 在启动时以及每 5 分钟运行一次
**通过 snell 隧道的 IPv6 可达性探测**：尝试拨号
`[2606:4700:4700::1111]:443`（Cloudflare DNS）。探测成功说明服务器
有可用的 v6 出口，到达 handler 的真实 v6 目标会被正常转发；探测失败则
立即丢弃 handler 上的真实 v6 目标，让 happy-eyeballs 应用在毫秒级回退到 A，
而不是卡在内核的 connect 超时上。

`ipv6 = false` 开关（在 `[snell-tun]` 下）会强制关闭 v6，**不考虑探测结果**
—— 用于探测目标无法判断、但服务器其实又用不了 v6 的场景，或者你纯粹想要
确定性的 v4-only 行为。

#### 分平台机制

| 关注点 | Linux | macOS |
| ------ | ----- | ----- |
| DNS 拦截 | `nftables` 将 UDP/TCP :53 DNAT 到 TUN 网关 | `networksetup -setdnsservers` 按网络服务改写 DNS（启动时记录，退出时恢复） |
| 到 fake-IP 的出站 TCP | TUN 设备上的 sing-tun "system" stack | 相同 |
| 到 real-IP 的出站 TCP | sing-tun `AutoRedirect`（nftables REDIRECT） | TUN auto-route 默认路由接管（8 条 sub-prefix） |
| 旁路 snell-client 自己到服务器的出站连接 | OUTPUT 链匹配 `SO_MARK 0x2024` | snell 服务器 IP 作为 `Inet4RouteExcludeAddress` 安装 |
| 保留主机上的入站服务 | 精确（基于 conntrack 的 PREROUTING 旁路） | 典型工作站场景可用；不要在接受公网入站连接的主机上开启 |
| 按进程排除（`realm`/`gost` 等） | `exclude-uid`（nftables UID 匹配） | 不支持（macOS 没有等价的内核 hook） |
| Direct IP / Direct Domain 旁路 | `netipx.IPSet` 喂给 `RouteExcludeAddressSet`；通过 `AutoRedirect.UpdateRouteAddressSet()` 实时重发布 | `/sbin/route add -host/-net … -interface <iface>`；最长前缀匹配胜过 utun auto-route 的半前缀。默认接口在启动时探测一次 |
| 进程异常退出后的残留路由清理 | 不需要（每次启动都整张 nftables 表重建） | 启动时回放 `/var/run/opensnell-bypass.state`，对每条记录执行 `route delete`，然后清空文件 |
| QUIC 快速失败 | 对 fake-IP 目标的 UDP/443 注入 ICMP Port Unreachable | 对 TUN 接住的所有目标的 UDP/443 注入 ICMP Port Unreachable |
| `SIGTERM`/`SIGINT` 清理 | 删除 nftables 表，移除 priority-1 `ip rule` | 销毁 utun，删除 auto-route，恢复系统 DNS，撤销 Direct 路由 |

DNS 拦截方式之所以不同，是因为 macOS 的 `mDNSResponder` 使用按网络服务划分的 DNS
作用域，会把查询绑定到 Wi-Fi/Ethernet 源接口并直接从该接口发出；单纯接管默认路由时，
这些 DNS 包根本不会经过 TUN。只有把配置的 DNS 改成 TUN 网关 IP（这个 IP 只存在于
utun 设备上），scoped routing 才会把查询送进 TUN。Linux 没有这套 scoping；
OUTPUT 链上的 nftables DNAT 会捕获所有查询，不受配置的 resolver 影响。

#### 按流量类型划分的行为

下面是按流量类型整理的速查表；平台差异在表中单独标出：

| 流量 | 行为 |
| ---- | ---- |
| 入站 TCP/UDP 到本机服务（sshd、nginx、caddy、realm 等） | **Linux：不变**（PREROUTING 排除本机目标地址）。**macOS：典型工作站场景下不变**，见下方 macOS 说明 |
| 这些服务返回给客户端的回包 | **Linux：不变**（ct mark 旁路 redirect）。**macOS：同样受下方注意事项影响** |
| 本机进程新发起的出站 TCP（按主机名） | **经 fake-IP 重定向** → snell 服务器收到 `AtypDomainName` |
| 本机进程新发起的出站 TCP（直接 IP） | **经 REDIRECT（Linux）/ TUN 默认路由（macOS）重定向** → snell 服务器收到 `AtypIPv4` |
| `snell-client` 自己连接 snell 服务器的 TCP | **Linux：不变**（OUTPUT 链匹配 SO_MARK 0x2024 后提前返回）。**macOS：不变**（server IP 会作为 `Inet4RouteExcludeAddress` 安装，内核不会把它送进 TUN） |
| AAAA DNS 查询 | 代理目标返回空 NOERROR（应用回退到 A）；Direct Domain 目标返回**真实记录**。没有 IPv6 fake-IP 路径 |
| 出站 IPv6 流量到**真实** v6 目标 | **Linux/macOS：受 v6 探测约束** — 只有当 snell 服务器有可用 v6 出口时才接受；否则丢弃，让应用回退 A。见上方"IPv6 策略" |
| 出站 UDP/443（Web QUIC） | **注入 ICMP Port Unreachable** 给应用，让 QUIC 立即放弃并回退到 TCP — 取代了原本"静默丢弃 → 等约 10 秒超时"的行为 |
| 本机进程的其他 UDP（DNS、443 以外） | **Linux：不变**（只接管 DNS；fake-IP UDP 静默丢弃）。**macOS：丢弃**（TUN 会接住所有 v4 UDP，v2 只处理 DNS） |
| 本机进程的 ICMP | **Linux：不变。** **macOS：丢弃** |
| `FORWARD` 链 / IP 转发流量 | **Linux：不变**（不改 FORWARD）。**macOS：不适用** |

在 **Linux** 上：任意来源的新 SSH 连接仍然可以进入，本机已有 TCP 服务会继续工作；
向 `snell-client` 发送 `SIGTERM`/`SIGINT` 后，TUN 接口、nftables 表以及 sing-tun
安装的那条 priority-1 `ip rule` 都会被删除，主机恢复到运行前的状态。端到端验证已经覆盖
在 ISP DNS 被污染的机器上 `docker pull` Docker Hub 官方镜像。

在 **macOS** 上：同样的 SIGTERM 清理还会把每个网络服务的 DNS 恢复到改写前状态。
auto-route 默认路由接管比 Linux 的 nftables REDIRECT 更粗粒度，因此**如果 macOS 主机上
运行着需要向远端客户端回包的入站服务，TUN 开启期间这些服务将无法正常工作**；回包会被拉进
TUN 并通过 snell 转发，而 snell 没有通向原始客户端的反向通道。对于典型 macOS 桌面用法，
也就是机器上没有接受入站连接的服务，这一点不会造成影响。如果你在 Mac 上运行对外服务，请不要开启 TUN。
已在使用被 ISP 污染的 DHCP DNS 的主机上完成 HTTPS 访问 Google 的端到端验证。

#### 环境要求

- **Linux**：内核已加载 `nf_tables` + `nf_conntrack` 模块（Debian 12+ /
  RHEL 9+ 默认满足），`nft` 用户态命令在 `$PATH` 中，并具备 `CAP_NET_ADMIN`
  （实际使用中通常以 root 运行，或通过 systemd `AmbientCapabilities` 授权）。
- **macOS**：需要 `sudo` 来创建 utun 设备、安装路由并改写系统 DNS。
  `networksetup` 是系统自带命令。
- 在两个平台上，同一份二进制的普通 SOCKS5 模式仍然可以在无 root 权限下使用。

#### 排除特定服务（仅 Linux）

如果同一台机器上还运行透明转发器（`realm`、`gost`、`socat` 一类端口转发工具），
通常应该让它们继续直连出站：snell 服务器并不是这个转发器的目标；这个转发器本身才是其他人的目标。

- Linux 的 nftables 只能按 socket 所属的 **UID** 或 **GID** 匹配。
  `PID` 不稳定，进程名 / `comm` 在 netfilter 层**完全不可见**；这是 Linux 内核层面的硬限制，
  适用于所有透明代理，包括 clash、sing-box、v2ray-redir、ss-redir 等。
- 标准做法是让转发器以独立用户运行。对 systemd 管理的服务，可以在 unit 中加入
  `User=realm`（并先用 `useradd -r realm` 创建专用系统用户）。
- 然后在下面的 `exclude-uid` 中列出这些用户名或 UID。

在 **macOS** 上没有这个开关；系统没有可以按 UID 匹配出站 socket 的等价内核 hook。
如果你需要在 Mac 上旁路某个特定进程，就要让该进程改用其他机制连接，例如直接走 SOCKS5、
单独的网络隔离方式等，而不是依赖 TUN 接管。

#### Direct IP / Direct Domain 旁路

除了"全部走 snell"之外，你也可以把特定目标标记为 **direct** —— 由主机
自身的路由栈处理，snell 完全看不到。适合 LAN 网段、公司内网、以及在代理后
行为异常的第三方服务。

两个粒度，都配置在 `[snell-tun]` 下：

- **`direct-ip`** —— 逗号分隔的 CIDR（`10.0.0.0/8, 192.168.0.0/16`）
  或裸 IP（`1.1.1.1, 8.8.8.8`）。静态，启动时加载。
- **`direct-domain`** —— 逗号分隔的 DNS 后缀列表
  （`internal.example.com, intranet.local`）。fake-IP DNS 服务器看到匹配
  的查询时，会把它原样转发给 `upstream-dns`，而不是合成 fake-IP；然后把
  回包里的每个 A/AAAA 地址按记录自带的 TTL 注册进旁路集。30 秒的回收
  goroutine 会清理过期项。

两个开关汇入同一套分平台旁路机制：

| 平台 | 旁路机制 |
| ---- | -------- |
| **Linux** | 加进 `netipx.IPSet`，再调 sing-tun 的 `AutoRedirect.UpdateRouteAddressSet()`，让 nftables 重发布 REDIRECT 排除集 |
| **macOS** | 调 `route add -host <ip> -interface <默认接口>`；内核最长前缀匹配胜过 sing-tun auto-route 的半前缀。默认接口启动时通过 `route -n get default` 探测一次 |

Direct Domain DNS 转发器在 Linux 上会给上游 UDP 查询打上
snell-client 的 `OutputMark`（SO_MARK 0x2024），让查询绕过 sing-tun 的
nft DNS 劫持；否则转发器会绕回我们自己的 fake-IP DNS 服务器，形成回环。

**跨崩溃的路由清理（macOS）**：旁路管理器每安装一条路由就同步写入
`/var/run/opensnell-bypass.state`（temp+rename 原子写）。启动时回放该
文件 —— 对每条记录执行 `route delete`，然后清空文件 —— 防止上一次
被 SIGKILL 的运行在内核路由表里留下没人认领的 /32 残留。Linux 不需要
等价机制：sing-tun 的 nftables 表本来每次启动就整张重建，排除集随之
重置。

示例：

```ini
[snell-tun]
enable = true

direct-ip = 10.0.0.0/8, 192.168.0.0/16, 1.1.1.1
direct-domain = corp.example.com, lan.local
upstream-dns = 223.5.5.5:53
```

启动后，`curl https://corp.example.com/internal` 会走主机原生路由直达
内网，而 `curl https://www.cloudflare.com/` 仍然经过 snell 隧道。

#### 配置

在原有 `[snell-client]` 之外添加一个 `[snell-tun]` 段：

```ini
[snell-tun]

; 总开关。默认 false — 不写或为 false 时，snell-client 的行为与以往的
; SOCKS5-only 构建完全一致。也可以通过 --tun 命令行参数强制开启。
enable = true

; TUN 接口名。默认 "snell0"。
;interface = snell0

; Fake-IP CIDR。默认 198.18.128.0/17（RFC 2544 benchmark 保留段的一个子集；
; 特意避开 clash 默认的 198.18.0.0/16 和 sing-box 默认的 198.18.0.0/15，
; 让 opensnell 可以与它们共存于同一台机器）。只有在确认存在冲突时才需要修改。
;fake-ip-range = 198.18.128.0/17

; MTU。默认 9000。
;mtu = 9000

; 逗号分隔的 UID 和/或用户名；这些用户发起的出站 TCP 会绕过重定向，
; 直接从机器原本的默认网关出站。用户名会在启动时通过系统 passwd 数据库解析。
; 仅 Linux —— macOS 没有等价的内核 hook。典型用法：让透明 TCP 转发器
; 以自己的专用用户运行。
;exclude-uid = realm, gost

; IPv6 总开关。默认 true。为 true（或不写）时，snell-client 在启动时
; 以及每 5 分钟通过隧道拨一次已知的 v6 端点，探测服务器的 v6 可达性；
; 只有探测说服务器能用 v6，TUN handler 才接受真实 v6 目标。设为 false
; 可以强制关闭 v6，**不考虑探测结果** —— 适合"探测目标可达但服务器
; 实际上又用不了 v6"的奇怪场景。
;ipv6 = true

; 覆盖 v6 可达性探测目标。格式 "[v6]:port"，
; 默认 [2606:4700:4700::1111]:443（Cloudflare DNS）。
;ipv6-probe-target = [2606:4700:4700::1111]:443

; v6 探测重跑间隔。接受 Go duration 字符串。默认 5 分钟。
;ipv6-probe-interval = 5m

; Direct IP —— 逗号分隔的 CIDR（裸 IP 视为 /32 或 /128），这些目标会
; 完全绕过代理，使用主机原本的路由。启动时静态加载。典型用途：内网
; 网段、企业 intranet、在代理后行为异常的特定服务。
;direct-ip = 10.0.0.0/8, 192.168.0.0/16, 1.1.1.1

; Direct Domain —— 逗号分隔的 DNS 后缀列表。匹配的查询会被转发给
; upstream-dns（拿到真实记录，不合成 fake-IP），返回的每个 A/AAAA
; 地址都会按记录自带的 TTL 注册进旁路集。后缀匹配规则：
; "example.com" 匹配 "example.com" 和 "foo.example.com"，
; 但不匹配 "notexample.com"。需要同时配置 upstream-dns。
;direct-domain = corp.example.com, lan.local

; direct-domain 查询的上游 DNS。格式 "ip:port"，:53 可省略。仅用于
; 匹配 direct-domain 后缀的查询；其他查询仍走 fake-IP 路径。无默认值
; —— 配置了 direct-domain 时必填。
;upstream-dns = 223.5.5.5:53
```

运行：

```sh
sudo ./snell-client --tun -c snell-client.conf
```

你可以保留 `[snell-client]` 中的 `listen = 127.0.0.1:1080`，让 SOCKS5 监听和
TUN 入站同时运行；也可以设置 `listen = off`，只运行 TUN。

#### 已知限制 / 后续工作

- **Fake-IPv6 池**：AAAA 查询目前返回空 NOERROR，让应用回退到 A。
  我们从不分配 fake-IPv6 地址。这意味着仅 v6（没有 A 记录）的目标
  无法走代理。临时解决：如果你有原生 v6 出口，可以把这类域名列入
  `direct-domain`；否则该域名必须 v4 可达。未来版本会在 v6 探测
  成功时启用 ULA fake-IPv6 池。

- **Linux IPv6 UDP 接管**：Linux 上只有 IPv4 UDP/443 流量会到达
  handler（TUN 只持有 fake-IPv4 CIDR；nft auto-redirect 仅接管 TCP）。
  真实 v6 UDP（包括 v6 QUIC）走主机原生接口，**不会**收到 ICMP
  回退包。实际上这只在"服务器没有 v6 但主机有"的情形下有意义 ——
  这种情况下应用直接用主机原生 v6 跑 QUIC 反而是预期行为。

- **macOS 默认接口热切换**：旁路管理器只在启动时探测一次默认物理
  接口。运行中 Wi-Fi 切到以太网时，已经安装的 Direct 路由还指向旧
  接口，会失效直到重启。订阅 sing-tun 默认接口监视器、在切换时
  自动重装路由在 TODO 列表里。

- **非 443 端口的 QUIC**：只有 UDP/443（HTTP/3 通用端口）会得到
  ICMP 注入。其他端口上的自定义 QUIC 应用仍然静默丢弃；对任意
  UDP 端口都注 ICMP 会让游戏 / VoIP / mDNS 受到误伤，比 10 秒
  超时还糟。

- **应用 UDP 流量代理**：仍未实现（DNS 和 UDP/443 ICMP 注入是
  我们对 UDP 做的全部）。WebRTC 等纯 UDP 应用在 Linux 上直连出站，
  macOS 上被丢弃。本 alpha 暂不涉及。

- **基于 cgroup-v2 的排除**（Linux）：现在按 UID 匹配旁路用户。
  systemd unit 加 `User=` 是临时方案。原生 cgroup-v2 路径分类
  可以让 unit 直接被豁免，未做。

- **Windows**：不支持。

### 端到端冒烟测试示例

```sh
# 另开一个终端，并确保 snell-client 正在 127.0.0.1:1080 上运行：
curl -sS --socks5-hostname 127.0.0.1:1080 https://www.cloudflare.com/cdn-cgi/trace
# 预期响应正文中的 `ip=` 行会显示 snell-server 的出口 IP。
```

## 将 OpenSnell 服务端与 Surge 配合使用（推荐用于 QUIC/HTTP-3）

在 Surge 配置中，将该服务端添加为 snell 代理，设置 `version=5`，并关闭 Surge
针对每条连接的 QUIC 阻断：

```ini
[Proxy]
my-snell = snell, your-server.example.com, 2333, psk=your-shared-secret, version=5, tfo=true, block-quic=off
```

当 Surge 通过 `my-snell` 分发 HTTP/3 连接时，它会把最初 1 到 2 个 QUIC Initial
数据包包裹在 snell 信封里（信封里包含目标 SNI/host，因此在线路上会被隐藏），之后的
其余数据包会以原始 QUIC 形式转发，由 `snell-server` 的 `ServeQUIC` 循环处理。

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

服务端解码第一个信封并记录 `(client_src, upstream)` 映射后，两个方向上的后续
UDP 数据包都会以**原始 QUIC**形式转发，不再附加任何 snell 帧封装。

该格式是通过抓取 Surge 客户端产生的真实 HTTP/3 流量，并使用配置中的 PSK 解密后，
对照官方 Surge `snell-server v5.0.1` 逆向得到的。详见
`components/snell/quic.go` 和 `components/snell/quic_test.go`；单元测试中包含一个
真实抓取到的 1359 字节信封作为 fixture。

## 性能

我们在两台同机房 Linux 主机上对比了 OpenSnell 与官方 `snell-server v5.0.1`
（也就是 Surge 客户端背后的闭源二进制）：其中一台主机在不同端口上**同时**运行两个服务端，
让两边共用同一条上游链路、同一套内核以及相同的 CDN 侧状态；另一台主机运行两个
snell-client 实例，分别指向这两个服务端。所有流量都通过 SOCKS5，经
`curl --socks5-hostname` 访问同一个上游 URL。

### 测试方法

测试分三组依次进行（从不同时运行）。每个被测对象之间都会停顿几秒，避免上游 CDN
对其中一方限速：

1. **延迟** — 对一个很小的端点连续请求 50 次
   （`cloudflare.com/cdn-cgi/trace`，响应约 200 B）。通过 `curl -w`
   测量 `time_connect`、TTFB 和总耗时。
2. **并发吞吐** — 以 N = 2、4、8 路并行下载一个 10 MB 文件。聚合 MB/s =
   总字节数 ÷ 墙钟时间。
3. **抓包分析** — 每个变体各下载一次 10 MB 文件，同时在服务端侧运行
   `tcpdump`，统计满载 TCP segment 与空 ACK 的数量。

### 官方二进制实际是什么

我们反汇编了官方 `snell-server-v5.0.1-linux-amd64`（1.2 MB，静态链接，
section headers 已剥离）。字符串分析显示它由 **GCC** 构建，链接了 **libuv**
（curl 与 Node.js 使用的同一套异步 I/O 库），并使用 **OpenSSL** 的 AES-NI GCM
实现（可以看到特征字符串 `GCM module for x86_64`）。简而言之，它是
**C/C++ + libuv + OpenSSL**。这一点很重要，因为 libuv 会把整个代理运行在单个
event-loop 线程上：没有按连接分配的 goroutine，没有 GMP 调度，也没有 GC。

### 初次结果（OpenSnell v1.0.1）

| 指标                                         | OpenSnell v1.0.1 | 官方 v5.0.1   | Δ           |
| -------------------------------------------- | ---------------: | ------------: | ----------- |
| TTFB 中位数                                  |       噪声范围内 |    噪声范围内 | ~0          |
| 单流吞吐                                     |             持平 |          持平 | ~0          |
| **N = 8 并发吞吐**                           |    **6.49 MB/s** | **8.46 MB/s** | **−30 %**   |
| 一次 10 MB 传输中的空 ACK 数                 |             1444 |          1084 | **+33 %**   |

单流吞吐和延迟已经与官方服务端基本一致，差距主要集中在并发吞吐上。

### 根因

`v4Reader.readFrame()` 反序列化每个 snell 帧时会进行**两次独立的
`io.ReadFull` 调用**：一次读取 23 字节的 AEAD 帧头，一次读取 padding +
payload + tag。底层 `net.Conn` 当时又是直接读取，没有用户态缓冲。按典型帧大小约
1.5 KB 计算，一次 10 MB 传输会经过约 7300 个帧，因此每个方向大约需要
**14000 次 `recv()` 系统调用**。

这会带来两个结果：

1. **空 ACK 增多。** Linux 在应用层大块排空接收缓冲区时会延迟 ACK，
   但如果缓冲区被很多小读不断排空，内核就会更积极地发送 ACK。每帧两个 syscall
   意味着大量小读，也就让 delayed ACK 失效，导致线路上的空 ACK 比 C 参考实现多约 33%。
2. **并发吞吐下降。** 每条 snell 连接会运行两个 goroutine（每个方向一个）。
   N = 8 个并发 SOCKS5 会话意味着 16 个 goroutine，它们都在执行大量小 syscall，
   并通过 Go runtime 调度切换。libuv 没有这部分开销；它的单个 epoll 驱动线程可以以满速接收新的 TCP 数据。

### 修复

只改一行：

```go
// components/snell/v4.go — initReader()
c.r = &v4Reader{Reader: bufio.NewReaderSize(c.Conn, 64*1024), aead: aead}
```

64 KB 的读侧缓冲可以让一次 `recv()` 把约 40 个最大尺寸的 snell 帧拉入用户态，
使读取路径上的系统调用数量大约减少 **90 倍**。这个改动对线路格式完全透明：
v4 帧解析器看到的仍是完全相同的字节流，只是这些字节通过更少的系统调用送达。

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
Go runtime 相对手写 C event loop 的额外开销，并且已经低于真实工作负载中的可感知噪声。

### 小结

在 Surge 已公开的 snell v5 线路协议上，OpenSnell 的 `snell-server` 在并发场景下可以达到
官方 C 参考实现**约 98% 的性能**，延迟表现则**几乎不可区分**。这次 bufio 修复在
`components/snell/v4.go` 中只有 `+9/−1` 行；它也说明，缩小与原生 C/libuv 实现之间的差距时，
最值得剖析的往往是读取路径，而不只是应用层逻辑。

## 实验性：tcp-brutal 拥塞控制

[apernet/tcp-brutal](https://github.com/apernet/tcp-brutal) 是一个 Linux
内核模块，会向系统注册名为 `brutal` 的 TCP 拥塞控制算法。用户态可以为每个 socket
设置固定发送速率；内核会按这个速率 pacing 发包，而不根据丢包反馈降速。它适用于高丢包的
长肥网络，在 cubic/bbr 吞吐塌陷时可能有用。

在 OpenSnell 中的使用方式：

```sh
# 在服务端宿主机（Linux）：
apt install linux-headers-$(uname -r) make gcc
git clone https://github.com/apernet/tcp-brutal /tmp/tcp-brutal
cd /tmp/tcp-brutal && make && insmod brutal.ko
# 验证是否加载成功：
cat /proc/sys/net/ipv4/tcp_available_congestion_control   # 应该包含 "brutal"
```

然后在 `snell-server.conf`（以及/或 `snell-client.conf`）中设置
`brutal = true` 和 `brutal-mbps = <Mbps>`。重启后，对应方向的每条连接都会被固定到该速率。

**端到端实测**：在支持 v6 的 Linux 服务器上设置 `brutal-mbps = 50`，
通过 `cachefly.cachefly.net/100mb.test` 下载 100 MB 文件：

| 服务端 CC       | 耗时    | 速率       |
| --------------- | ------- | ---------- |
| vanilla (cubic) | 1.89 s  | 444 Mbps   |
| brutal (50)     | 16.97 s | **49.4 Mbps**（与配置的 50 相差 < 2%） |

### ⚠️ 开启 brutal 之前必须读

上游维护者的原话：

> "Brutal 的速率设置会**作用于每一条独立连接**。这让它只适合支持多路复用（mux）的协议。
> 对每个代理连接都需要单独 TCP 连接的协议来说，如果多个连接同时活跃，使用 TCP Brutal 会**压垮接收端**。"

Snell **不支持 mux**。reuse 模式（`CommandConnectV2`）是**串行**的，
也就是一条 TCP 同一时间只承载一个 CONNECT；而客户端仍然可能同时持有多条池化 TCP。
**如果你的工作负载会同时打开 N 条 snell 连接，服务端会尝试以 N × `brutal-mbps`
的总速率推送**，接收端可能会因为丢包而崩溃。只有在以下场景才应使用：

- 你有持续的单流工作负载（大文件下载、视频流等），或者
- 你正在测试。

两端可以独立启用 brutal。只在服务端启用时，控制 server → client 方向
（下载，也是典型代理流量）；只在客户端启用时，控制 client → server 方向（上传）。
两端配置不需要相同。

## 与真实 Surge `snell-server` 的互通性

已针对 `snell-server v5.0.1 (Nov 19 2025)` 完成测试：

| 路径                                      | 结果                                             |
| ----------------------------------------- | ------------------------------------------------ |
| 我方客户端 → 真实服务端，TCP              | ✅ 10/10                                         |
| 我方客户端 → 真实服务端，UDP-over-TCP     | ✅ DNS 往返成功                                  |
| 我方客户端 → 真实服务端，复用             | ✅ 30 次串行 + 20 次并发                         |
| 我方服务端，QUIC 模式，真实 Surge 信封    | ✅ 基于真实抓包的单元测试通过                    |
| HTTP/3 → 我方服务端 → Cloudflare          | ✅ 5/5（`ip=` 回显我方服务端，`sni=plaintext`）  |

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
