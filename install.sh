#!/usr/bin/env bash
#
# OpenSnell server installer
#
# Subcommands:
#   install        Interactive install (default action)
#   reconfigure    Rewrite config interactively, restart service
#   update         Update binary in place
#   uninstall      Stop service + remove binary + (optional) remove config
#   start | stop | restart | enable | disable | status
#   info           Show connection details for the currently-installed server
#   help
#
# Without arguments, an interactive menu is shown.
#
# Three install variants:
#   1) OpenSnell (default, GPLv3, all-platform, this repo)
#   2) Surge official snell-server v5.0.1 (closed-source, Linux only)
#   3) Surge official snell-server v6.0.0b3 (closed-source beta, Linux only,
#      protocol v6; statically linked — no extra shared libraries needed;
#      adds the `mode` directive: default / unshaped / unsafe-raw)
#
# Project: https://github.com/missuo/opensnell
# SPDX-License-Identifier: GPL-3.0-or-later

set -uo pipefail

# ============================================================================
# Pretty-printing
# ============================================================================
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
BLUE='\033[0;34m'; MAGENTA='\033[0;35m'; CYAN='\033[0;36m'
BOLD='\033[1m'; NC='\033[0m'

print_header()  { echo; echo -e "${BOLD}${BLUE}===========================================================${NC}"; echo -e "${BOLD}${BLUE}  $1${NC}"; echo -e "${BOLD}${BLUE}===========================================================${NC}"; echo; }
print_success() { echo -e "${GREEN}[OK]${NC} $1"; }
print_error()   { echo -e "${RED}[ERROR]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARN]${NC} $1"; }
print_info()    { echo -e "${CYAN}[INFO]${NC} $1"; }

# ============================================================================
# Paths
# ============================================================================
INSTALL_BIN="/usr/local/bin/snell-server"
CONFIG_DIR="/etc/snell"
CONFIG_FILE="$CONFIG_DIR/snell-server.conf"
META_FILE="$CONFIG_DIR/.install_meta"
SERVICE_NAME="snell-server"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

OPENSNELL_REPO="missuo/opensnell"
OPENSNELL_RELEASE_API="https://api.github.com/repos/${OPENSNELL_REPO}/releases/latest"
SURGE_V5_VERSION="v5.0.1"
SURGE_V6_VERSION="v6.0.0b3"
SURGE_BASE_URL="https://dl.nssurge.com/snell"

# ============================================================================
# Preflight
# ============================================================================
check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        print_error "This script must be run as root (sudo bash install.sh ...)."
        exit 1
    fi
}

# This installer is Linux-only: it depends on systemd for service
# management, iptables/UFW/firewalld for the port hole-punch, and
# /etc/sysctl.conf for TFO. Anything else is out of scope — call out
# the alternatives early instead of failing halfway through.
check_linux() {
    if [ "$(uname -s)" != "Linux" ]; then
        print_error "This installer is Linux-only."
        print_info  "Detected OS: $(uname -s) — not supported."
        print_info  "On macOS / Windows / *BSD, build from source instead:"
        print_info  "    go install github.com/missuo/opensnell/cmd/snell-server@latest"
        print_info  "and configure / supervise it with your platform's native tooling"
        print_info  "(launchd, NSSM, rc.d, etc.). The OpenSnell server itself is"
        print_info  "cross-platform; only this installer is Linux-specific."
        exit 1
    fi
}

detect_arch_opensnell() {
    case "$(uname -m)" in
        x86_64)         echo "amd64"  ;;
        aarch64|arm64)  echo "arm64"  ;;
        i386|i686)      echo "386"    ;;
        armv7l|armv7)   echo "armv7"  ;;
        *) print_error "Unsupported architecture: $(uname -m)"; exit 1 ;;
    esac
}

detect_arch_surge() {
    case "$(uname -m)" in
        x86_64)         echo "amd64"   ;;
        aarch64|arm64)  echo "aarch64" ;;
        i386|i686)      echo "i386"    ;;
        armv7l|armv7)   echo "armv7l"  ;;
        *) print_error "Surge official binary not available for $(uname -m)."; exit 1 ;;
    esac
}

ensure_tools() {
    # unzip only needed for the Surge variant. curl/openssl/ss are core.
    local missing=()
    for t in curl unzip openssl ss; do
        command -v "$t" >/dev/null 2>&1 || missing+=("$t")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        print_info "Installing missing tools: ${missing[*]}"
        if command -v apt-get >/dev/null 2>&1; then
            apt-get update -qq && apt-get install -y "${missing[@]}" || {
                print_error "Failed to install: ${missing[*]}"; exit 1; }
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y "${missing[@]}" || { print_error "Failed to install"; exit 1; }
        elif command -v yum >/dev/null 2>&1; then
            yum install -y "${missing[@]}" || { print_error "Failed to install"; exit 1; }
        else
            print_error "Unsupported package manager. Install manually: ${missing[*]}"
            exit 1
        fi
    fi
}

# ============================================================================
# Helpers
# ============================================================================
gen_psk() { openssl rand -base64 18 | tr -d '/+=' | cut -c1-24; }

base64_encode() {
    if command -v base64 >/dev/null 2>&1; then
        printf '%s' "$1" | base64 | tr -d '\n'
    else
        printf '%s' "$1" | openssl base64 -A
    fi
}

url_encode() {
    local byte dec ch byte_upper out=""
    for byte in $(printf '%s' "$1" | od -An -tx1 -v); do
        dec=$((16#$byte))
        if { [ "$dec" -ge 48 ] && [ "$dec" -le 57 ]; } \
            || { [ "$dec" -ge 65 ] && [ "$dec" -le 90 ]; } \
            || { [ "$dec" -ge 97 ] && [ "$dec" -le 122 ]; } \
            || [ "$dec" -eq 45 ] || [ "$dec" -eq 46 ] \
            || [ "$dec" -eq 95 ] || [ "$dec" -eq 126 ]; then
            printf -v ch '%b' "\\x${byte}"
            out+="$ch"
        else
            printf -v byte_upper '%02X' "$dec"
            out+="%${byte_upper}"
        fi
    done
    printf '%s' "$out"
}

yaml_double_quote_escape() {
    local value="${1//\\/\\\\}"
    value="${value//\"/\\\"}"
    printf '%s' "$value"
}

# Pick a free random port in [10000, 60000] that's not occupied (TCP + UDP).
pick_free_port() {
    local p
    for _ in $(seq 1 50); do
        p=$(( RANDOM % 50000 + 10000 ))
        if ! ss -lnt -lnu 2>/dev/null | awk '{print $5}' | grep -qE "[:.]$p\$"; then
            echo "$p"; return 0
        fi
    done
    print_error "Could not find a free port after 50 tries"
    return 1
}

get_ipv4() {
    curl -s -4 --max-time 5 ifconfig.me 2>/dev/null \
        || curl -s -4 --max-time 5 ip.sb 2>/dev/null \
        || curl -s -4 --max-time 5 ipinfo.io/ip 2>/dev/null \
        || echo "YOUR_SERVER_IP"
}

# cc_to_flag converts a 2-letter ISO country code to the corresponding
# regional-indicator flag emoji. e.g. "JP" -> "🇯🇵". Returns nothing on
# invalid input. Two-step: build the format string (with the hex
# codepoints inlined) and then printf -e it, because `printf '\U%08x'`
# does not work — `\U` must be followed by literal hex digits in the
# format string, not a placeholder.
cc_to_flag() {
    local cc
    cc=$(printf '%s' "${1:-}" | tr '[:lower:]' '[:upper:]')
    [ "${#cc}" = 2 ] || return 0
    case "$cc" in *[!A-Z]*) return 0 ;; esac
    local a_ord b_ord hex_a hex_b
    a_ord=$(printf '%d' "'${cc:0:1}")
    b_ord=$(printf '%d' "'${cc:1:1}")
    hex_a=$(printf '%08x' $(( 0x1F1E6 + a_ord - 65 )))
    hex_b=$(printf '%08x' $(( 0x1F1E6 + b_ord - 65 )))
    # The format string now contains literal hex digits after \U, so
    # bash's printf can interpret it as a Unicode codepoint pair.
    printf "\\U${hex_a}\\U${hex_b}"
}

# fetch_geo populates GEO_IP and GEO_COUNTRY in two steps:
#   1) get our public IPv4 from ip.sb (plain text response)
#   2) look up that IP via api.ipinfo.es for the country code
# Both steps have short timeouts. On total failure GEO_IP / GEO_COUNTRY
# stay empty and generate_node_name falls back to a generic random name.
fetch_geo() {
    GEO_IP=""
    GEO_COUNTRY=""
    GEO_IP=$(curl -s -4 --max-time 5 https://ip.sb 2>/dev/null | tr -d '[:space:]')
    # Validate looks like an IPv4 octet quartet (be conservative).
    case "$GEO_IP" in
        *[!0-9.]*|"") GEO_IP="" ;;
    esac
    [ -z "$GEO_IP" ] && return 0
    local json
    json=$(curl -s -4 --max-time 5 "https://api.ipinfo.es/ipinfo?ip=${GEO_IP}" 2>/dev/null || true)
    GEO_COUNTRY=$(echo "$json" | grep -o '"country":"[^"]*"' | head -1 | cut -d'"' -f4)
    case "$GEO_COUNTRY" in *[!A-Za-z]*) GEO_COUNTRY="" ;; esac
}

# generate_node_name produces a Surge-style label such as "🇯🇵 JP A1B2".
# Requires fetch_geo to have been called first. Falls back to a generic
# "OpenSnell <random4>" when geo lookup yielded no country code, so the
# installer never fails for lack of geo data.
generate_node_name() {
    local flag suffix
    flag=$(cc_to_flag "${GEO_COUNTRY:-}")
    suffix=$(LC_ALL=C tr -dc 'A-Z0-9' </dev/urandom 2>/dev/null | head -c 4)
    [ -z "$suffix" ] && suffix=$(date +%s | tail -c 5)
    if [ -n "$flag" ] && [ -n "${GEO_COUNTRY:-}" ]; then
        printf '%s %s %s' "$flag" "$GEO_COUNTRY" "$suffix"
    elif [ -n "${GEO_COUNTRY:-}" ]; then
        printf '%s %s' "$GEO_COUNTRY" "$suffix"
    else
        printf 'OpenSnell %s' "$suffix"
    fi
}

# Make sure the kernel has TFO fully enabled (bit 0 + bit 1 = 3).
# The server itself (OpenSnell, or the official Surge binary — verified to
# setsockopt(SOL_TCP, TCP_FASTOPEN) on its listener in both v5.0.1 and
# v6.0.0b3) opens the socket with TFO; this sysctl is the kernel gate that
# makes the server bit actually engage. If the running kernel already reports
# 3 we DON'T touch the user's system. We only edit /etc/sysctl.conf when the
# kernel is missing one of the bits, and even then we keep the edit minimal
# (sed-or-append).
enable_tfo_sysctl() {
    local current
    current=$(sysctl -n net.ipv4.tcp_fastopen 2>/dev/null || echo "")

    if [ "$current" = "3" ]; then
        print_info "Kernel net.ipv4.tcp_fastopen=3 already; no system changes needed"
        return 0
    fi

    print_warning "Kernel net.ipv4.tcp_fastopen=${current:-unknown} (TFO needs 3)"
    local confirm
    confirm=$(prompt_yesno "Set it to 3 (will write to /etc/sysctl.conf and apply via sysctl -p)" "y")
    if [ "$confirm" != "y" ]; then
        print_warning "Skipping; the server will still set the socket option, but TFO won't actually take effect"
        return 0
    fi

    local sysctl_conf="/etc/sysctl.conf"
    local setting="net.ipv4.tcp_fastopen = 3"
    if [ -f "$sysctl_conf" ] && grep -qE '^[[:space:]]*net\.ipv4\.tcp_fastopen' "$sysctl_conf"; then
        sed -i "s|^[[:space:]]*net\.ipv4\.tcp_fastopen.*|$setting|" "$sysctl_conf"
        print_success "Updated existing entry in $sysctl_conf"
    else
        echo "$setting" >> "$sysctl_conf"
        print_success "Appended to $sysctl_conf"
    fi

    if sysctl -p >/dev/null 2>&1; then
        local after
        after=$(sysctl -n net.ipv4.tcp_fastopen 2>/dev/null || echo "?")
        if [ "$after" = "3" ]; then
            print_success "Kernel now reports net.ipv4.tcp_fastopen=3"
        else
            print_warning "sysctl -p ran but kernel reports tcp_fastopen=$after (expected 3)"
        fi
    else
        print_warning "sysctl -p failed; reboot or run it manually to apply"
    fi
}

get_installed_version() {
    [ -f "$META_FILE" ] && grep '^version=' "$META_FILE" | cut -d= -f2 || true
}

get_install_variant() {
    [ -f "$META_FILE" ] && grep '^variant=' "$META_FILE" | cut -d= -f2 || true
}

prompt_default() {
    # prompt_default "Question" "default" -> echoes user input or default
    local question="$1" default="$2" reply
    if [ -n "$default" ]; then
        read -r -p "$(echo -e "${CYAN}${question} [${BOLD}${default}${NC}${CYAN}]: ${NC}")" reply
    else
        read -r -p "$(echo -e "${CYAN}${question}: ${NC}")" reply
    fi
    echo "${reply:-$default}"
}

prompt_yesno() {
    # prompt_yesno "Question" "y" -> echoes "y" or "n"
    local question="$1" default="$2" reply
    read -r -p "$(echo -e "${CYAN}${question} (y/n) [${BOLD}${default}${NC}${CYAN}]: ${NC}")" reply
    reply="${reply:-$default}"
    case "${reply,,}" in y|yes) echo "y" ;; *) echo "n" ;; esac
}

# ============================================================================
# Download + install binary
# ============================================================================
download_opensnell() {
    print_header "Downloading OpenSnell"
    mkdir -p "$CONFIG_DIR"
    local arch tag url
    arch=$(detect_arch_opensnell)
    tag=$(curl -fsSL "$OPENSNELL_RELEASE_API" | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
    if [ -z "$tag" ]; then
        print_error "Could not resolve latest release from GitHub API."
        print_info "Build from source instead: go install github.com/${OPENSNELL_REPO}/cmd/snell-server@latest"
        exit 1
    fi
    # The release publishes raw, per-target binaries — no archive wrapper.
    # Linux assets are unsuffixed; Windows ones end in .exe but we don't
    # consume those here (this installer is Linux-only by design).
    url="https://github.com/${OPENSNELL_REPO}/releases/download/${tag}/snell-server-linux-${arch}"

    print_info "Variant:      OpenSnell (self-hosted, GPLv3)"
    print_info "Architecture: linux/${arch}"
    print_info "Version:      ${tag}"
    print_info "Source:       ${url}"

    # Use explicit cleanup rather than a RETURN trap. A non-`-T` RETURN
    # trap stays armed *globally* after this function returns, so every
    # subsequent function-return fires it again — and by then $tmp is
    # out of scope, which under `set -u` blows up with "tmp: unbound
    # variable". Explicit cleanup is simpler and side-effect-free.
    local tmp
    tmp=$(mktemp)
    if ! curl -fL --progress-bar -o "$tmp" "$url"; then
        rm -f "$tmp"
        print_error "Download failed."
        print_info "If this is the first release, the GitHub Actions build may not have produced binaries yet."
        print_info "You can also build from source: go install github.com/${OPENSNELL_REPO}/cmd/snell-server@latest"
        exit 1
    fi
    install -m 0755 "$tmp" "$INSTALL_BIN"
    rm -f "$tmp"
    print_success "Installed OpenSnell ${tag} → ${INSTALL_BIN}"

    echo "variant=opensnell" >  "$META_FILE.tmp"
    echo "version=$tag"      >> "$META_FILE.tmp"
    echo "proto=5"           >> "$META_FILE.tmp"
}

# About v6.0.0b3 (measured, not theoretical): v6 adds a PSK-derived per-frame
# traffic-shaping layer (a padding keystream + padding↔ciphertext interleave on
# top of AES-GCM) for anti-fingerprinting. b2 fixed the two big drawbacks of the
# b1 beta: it is statically linked (no extra shared libraries) and multi-core
# (SO_REUSEPORT + io_uring workers), so it no longer saturates a single core — it
# reaches the ~52 MB/s co-located link ceiling at ~10% CPU, where b1 capped at
# ~30 MB/s burning one core. b3 adds the `mode` directive — `default` (obfs +
# AES, unchanged), `unshaped` (AES only, no shaping: ~10% more throughput, wire
# looks fully random like v3), and `unsafe-raw` (plaintext, no crypto — trusted
# tunnels only). It is still a closed-source beta, so for maximum stability
# OpenSnell (option 1) or Surge v5.0.1 (option 2) remain the safe picks.
note_v6_beta() {
    print_warning "snell-server ${SURGE_V6_VERSION} is a closed-source BETA."
    print_info  "v6 adds a PSK-derived per-frame traffic-shaping layer (anti-fingerprinting)."
    print_info  "b2 made it statically linked (no extra libs) and multi-core; b3 adds the"
    print_info  "\`mode\` directive (default / unshaped / unsafe-raw) — you'll pick one next."
    print_info  "For maximum stability, OpenSnell (option 1) or Surge ${SURGE_V5_VERSION} (option 2) are safe picks."
    local go_on
    go_on=$(prompt_yesno "Install ${SURGE_V6_VERSION}?" "y")
    if [ "$go_on" != "y" ]; then
        print_info "Aborted. Re-run and pick option 1 or 2."
        exit 0
    fi
}

download_surge() {
    # download_surge <version>, e.g. "v5.0.1" or "v6.0.0b3".
    local version="$1" proto="5"
    case "$version" in v6*) proto="6" ;; esac

    print_header "Downloading Surge official snell-server"
    mkdir -p "$CONFIG_DIR"
    local arch url workdir
    arch=$(detect_arch_surge)
    # v6 dropped the armv7l build; only amd64 / i386 / aarch64 are published.
    if [ "$proto" = "6" ] && [ "$arch" = "armv7l" ]; then
        print_error "Surge snell-server ${version} is not available for armv7l."
        print_info  "Choose the v5 variant or the OpenSnell variant instead."
        exit 1
    fi
    url="${SURGE_BASE_URL}/snell-server-${version}-linux-${arch}.zip"

    print_info "Variant:      Surge official (closed-source, Linux only)"
    print_info "Architecture: linux/${arch}"
    print_info "Version:      ${version}"
    print_info "Source:       ${url}"
    print_warning "By proceeding you accept Surge's license terms."

    # Same reasoning as download_opensnell: explicit cleanup, no trap.
    workdir=$(mktemp -d)
    if ! curl -fL --progress-bar -o "$workdir/snell.zip" "$url"; then
        rm -rf "$workdir"
        print_error "Download failed from $url"
        exit 1
    fi
    unzip -q "$workdir/snell.zip" -d "$workdir"
    install -m 0755 "$workdir/snell-server" "$INSTALL_BIN"
    rm -rf "$workdir"
    print_success "Installed Surge snell-server ${version} → ${INSTALL_BIN}"

    if [ "$proto" = "6" ]; then
        # v6.0.0b3 is statically linked, so no shared-library setup is needed.
        # It is UPX-packed (ldd can't enumerate it), so the reliable check is to
        # actually execute it.
        if "$INSTALL_BIN" -v >/dev/null 2>&1; then
            print_success "Binary runs OK ($("$INSTALL_BIN" -v 2>&1 | grep -o 'snell-server v[^ ]*'))"
        else
            print_error "snell-server ${version} fails to start."
            print_info  "Run '$INSTALL_BIN -v' to see the error."
            exit 1
        fi
    fi

    echo "variant=surge"       >  "$META_FILE.tmp"
    echo "version=${version}"  >> "$META_FILE.tmp"
    echo "proto=${proto}"      >> "$META_FILE.tmp"
}

# ============================================================================
# Interactive config builder
# ============================================================================
build_config() {
    # Prefer the variant set during the immediately-preceding download (we
    # haven't committed META_FILE yet), or fall back to existing meta on
    # reconfigure.
    local variant="" proto=""
    if [ -f "$META_FILE.tmp" ]; then
        variant=$(grep '^variant=' "$META_FILE.tmp" | cut -d= -f2)
        proto=$(grep '^proto='     "$META_FILE.tmp" | cut -d= -f2)
    elif [ -f "$META_FILE" ]; then
        variant=$(grep '^variant=' "$META_FILE" | cut -d= -f2)
        proto=$(grep '^proto='     "$META_FILE" | cut -d= -f2)
    fi
    variant="${variant:-opensnell}"
    proto="${proto:-5}"

    print_header "Snell server configuration"
    mkdir -p "$CONFIG_DIR"

    # --- Port ---
    local default_port port
    default_port=$(pick_free_port)
    port=$(prompt_default "Listen port (leave blank for a random free port)" "$default_port")
    if ! [[ "$port" =~ ^[0-9]+$ ]] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
        print_error "Invalid port: $port"; exit 1
    fi

    # --- PSK ---
    local psk
    psk=$(prompt_default "PSK / shared secret (leave blank to auto-generate)" "")
    if [ -z "$psk" ]; then
        psk=$(gen_psk)
        print_info "Generated PSK: ${BOLD}${psk}${NC}"
    fi
    # The v6 server hard-rejects PSKs outside 16..255 bytes at startup,
    # so catch it here instead of leaving a service that won't boot.
    if [ "$proto" = "6" ]; then
        local psk_len; psk_len=$(printf '%s' "$psk" | wc -c | tr -d ' ')
        if [ "$psk_len" -lt 16 ] || [ "$psk_len" -gt 255 ]; then
            print_error "snell v6 requires a PSK between 16 and 255 bytes (got ${psk_len})."
            exit 1
        fi
    fi

    # --- mode (v6.0.0b3+ only) ---
    # default    : obfuscation + AES encryption (the canonical mode; omittable).
    # unshaped   : AES only, no traffic shaping — ~10% more throughput; the wire
    #              looks fully random, equivalent to Snell v3.
    # unsafe-raw : no encryption AND no shaping — plaintext. Only safe inside a
    #              trusted intranet or wrapped in another secure tunnel.
    # The client MUST be configured with the same mode or the connection fails.
    local mode="default"
    if [ "$proto" = "6" ]; then
        mode=$(prompt_default "Traffic mode (default/unshaped/unsafe-raw)" "default")
        case "$mode" in
            default|unshaped) ;;
            unsafe-raw)
                print_warning "unsafe-raw forwards ALL traffic in PLAINTEXT — no encryption, no obfs."
                print_warning "Only use it inside a trusted intranet or another secure tunnel."
                ;;
            *) print_error "mode must be one of: default, unshaped, unsafe-raw"; exit 1 ;;
        esac
    fi

    # --- obfs (removed in v6; the v6 server silently ignores it) ---
    local obfs="off"
    if [ "$proto" != "6" ]; then
        obfs=$(prompt_default "obfs mode (off/http/tls)" "off")
        case "$obfs" in off|http|tls) ;; *) print_error "obfs must be one of: off, http, tls"; exit 1 ;; esac
    fi

    # --- ipv6 / DNS preference ---
    # v6 deprecates `ipv6 = true/false` in favour of dns-ip-preference
    # (default / prefer-ipv4 / prefer-ipv6 / ipv4-only / ipv6-only).
    local ipv6_choice ipv6="true" dns_pref="" dns_servers=""
    if [ "$proto" = "6" ]; then
        dns_pref=$(prompt_default "DNS IP preference (default/prefer-ipv4/prefer-ipv6/ipv4-only/ipv6-only)" "default")
        case "$dns_pref" in
            default|prefer-ipv4|prefer-ipv6|ipv4-only|ipv6-only) ;;
            *) print_error "Invalid DNS IP preference: $dns_pref"; exit 1 ;;
        esac
        [ "$dns_pref" = "ipv4-only" ] && ipv6="false"
        dns_servers=$(prompt_default "Custom DNS servers, comma-separated (leave blank for system default)" "")
    else
        ipv6_choice=$(prompt_yesno "Allow IPv6 destinations (server-side outbound)" "y")
        if [ "$ipv6_choice" = "y" ]; then ipv6="true"; else ipv6="false"; fi
    fi

    # Per-variant knobs.
    local udp="true" quic="true" egress="" tfo="false"
    if [ "$variant" = "opensnell" ]; then
        local udp_choice quic_choice tfo_choice
        udp_choice=$(prompt_yesno "Accept UDP-over-TCP (snell datagram protocol)" "y")
        [ "$udp_choice" = "n" ] && udp="false"

        quic_choice=$(prompt_yesno "Enable QUIC proxy mode (UDP on the same port; required for Surge HTTP/3)" "y")
        [ "$quic_choice" = "n" ] && quic="false"

        egress=$(prompt_default "Egress interface to pin upstream sockets to (leave blank for default route)" "")

        tfo_choice=$(prompt_yesno "Enable TCP Fast Open (saves 1 RTT per fresh connection; Linux only)" "n")
        [ "$tfo_choice" = "y" ] && tfo="true"
    else
        # Official Surge variant (v5 or v6). The official binary ALWAYS sets
        # TCP_FASTOPEN on its listener (verified via strace on both v5.0.1 and
        # v6.0.0b3: setsockopt(SOL_TCP, TCP_FASTOPEN) right before listen()),
        # so there is no server-side config knob — the listener TFO is built in.
        # It only pays off once the host kernel has the server bit set, so we
        # default to flipping net.ipv4.tcp_fastopen=3 and advertising tfo=true
        # to clients.
        if [ "$proto" = "6" ]; then
            # The official v6 server grew its own egress-interface directive.
            egress=$(prompt_default "Egress interface to pin upstream sockets to (leave blank for default route)" "")
        fi
        local tfo_choice
        tfo_choice=$(prompt_yesno "Enable TCP Fast Open (the official server already listens with it; this sets the host kernel switch and adds tfo=true to your client config)" "y")
        [ "$tfo_choice" = "y" ] && tfo="true"
    fi

    # --- Write config ---
    if [ "$proto" = "6" ]; then
        # Canonical v6 layout (what `snell-server --wizard` generates):
        # obfs is gone and ipv6 is deprecated, replaced by dns-ip-preference.
        # `mode` is new in v6.0.0b3 (default when omitted, but we write it
        # explicitly so the generated config documents the choice).
        cat > "$CONFIG_FILE" <<EOF
[snell-server]
listen = 0.0.0.0:${port}
psk = ${psk}
mode = ${mode}
dns-ip-preference = ${dns_pref}
EOF
        [ -n "$dns_servers" ] && echo "dns = ${dns_servers}"        >> "$CONFIG_FILE"
        [ -n "$egress" ]      && echo "egress-interface = ${egress}" >> "$CONFIG_FILE"
    else
        cat > "$CONFIG_FILE" <<EOF
[snell-server]
listen = 0.0.0.0:${port}
psk = ${psk}
obfs = ${obfs}
ipv6 = ${ipv6}
EOF
        if [ "$variant" = "opensnell" ]; then
            cat >> "$CONFIG_FILE" <<EOF
udp = ${udp}
quic = ${quic}
egress-interface = ${egress}
tfo = ${tfo}
EOF
        fi
    fi

    # Every supported server (OpenSnell and the official Surge binary) opens
    # its listener with the TFO socket option; net.ipv4.tcp_fastopen is the
    # kernel gate that makes the server bit engage. Apply it whenever this
    # install opted into TFO, regardless of variant.
    if [ "$tfo" = "true" ]; then
        enable_tfo_sysctl
    fi
    chmod 600 "$CONFIG_FILE"
    print_success "Configuration written to $CONFIG_FILE"

    # --- Resolve public IP, country, and node name for the Surge proxy
    # line printed at the end. Best-effort: if api.ip.sb is unreachable
    # we still publish a useful (IP-only) name.
    print_info "Resolving public IP via ip.sb..."
    fetch_geo
    local node_name
    node_name=$(generate_node_name)
    print_info "Public IP: ${BOLD}${GEO_IP}${NC}${GEO_COUNTRY:+  Country: ${GEO_COUNTRY}}"
    print_info "Node name: ${BOLD}${node_name}${NC}"

    # --- Persist meta ---
    if [ -f "$META_FILE.tmp" ]; then mv "$META_FILE.tmp" "$META_FILE"; fi
    {
        grep -vE '^(port|psk|obfs|ipv6|tfo|mode|dns_pref|node_name|geo_ip|geo_country)=' "$META_FILE" 2>/dev/null || true
        echo "port=$port"
        echo "psk=$psk"
        echo "obfs=$obfs"
        echo "ipv6=$ipv6"
        echo "tfo=$tfo"
        echo "mode=$mode"
        echo "dns_pref=$dns_pref"
        echo "node_name=$node_name"
        echo "geo_ip=$GEO_IP"
        echo "geo_country=$GEO_COUNTRY"
    } > "${META_FILE}.tmp" && mv "${META_FILE}.tmp" "$META_FILE"
    chmod 600 "$META_FILE"
}

# ============================================================================
# systemd unit
# ============================================================================
write_systemd_unit() {
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Snell server
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_BIN} -c ${CONFIG_FILE}
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    print_success "systemd unit installed → $SERVICE_FILE"
}

# ============================================================================
# Firewall
# ============================================================================
configure_firewall() {
    [ -f "$META_FILE" ] || return 0
    local port quic
    port=$(grep '^port=' "$META_FILE" | cut -d= -f2)
    [ -z "$port" ] && return 0
    quic=$(grep -E '^quic\s*=' "$CONFIG_FILE" 2>/dev/null | awk -F= '{gsub(/ /,"",$2); print $2}')

    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
        print_info "UFW detected; allowing $port"
        ufw allow "${port}/tcp" >/dev/null 2>&1 || true
        [ "$quic" = "true" ] && ufw allow "${port}/udp" >/dev/null 2>&1 || true
        print_success "UFW: TCP/${port}${quic:+ + UDP/${port}} opened"
    elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        print_info "firewalld detected; allowing $port"
        firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null 2>&1 || true
        [ "$quic" = "true" ] && firewall-cmd --permanent --add-port="${port}/udp" >/dev/null 2>&1 || true
        firewall-cmd --reload >/dev/null 2>&1 || true
        print_success "firewalld: TCP/${port}${quic:+ + UDP/${port}} opened"
    else
        print_info "No firewall manager active; skipping firewall configuration"
    fi
}

# ============================================================================
# Service lifecycle
# ============================================================================
start_service()   { systemctl start   "$SERVICE_NAME" && print_success "Service started";   }
stop_service()    { systemctl stop    "$SERVICE_NAME" && print_success "Service stopped";   }
restart_service() { systemctl restart "$SERVICE_NAME" && print_success "Service restarted"; }
enable_service()  { systemctl enable  "$SERVICE_NAME" >/dev/null 2>&1 && print_success "Service enabled (auto-start on boot)"; }
disable_service() { systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 && print_success "Service disabled"; }
status_service()  {
    print_header "Service Status"
    systemctl status "$SERVICE_NAME" --no-pager 2>&1 | head -15 || true
}

# ============================================================================
# Connection info
# ============================================================================
show_info() {
    if [ ! -f "$META_FILE" ]; then
        print_warning "No installation metadata found; is the server installed?"
        return 1
    fi
    local variant version proto port psk obfs ipv6 tfo mode dns_pref node_name geo_ip ip
    variant=$(grep   '^variant='     "$META_FILE" | cut -d= -f2-)
    version=$(grep   '^version='     "$META_FILE" | cut -d= -f2-)
    proto=$(grep     '^proto='       "$META_FILE" | cut -d= -f2-)
    port=$(grep      '^port='        "$META_FILE" | cut -d= -f2-)
    psk=$(grep       '^psk='         "$META_FILE" | cut -d= -f2-)
    obfs=$(grep      '^obfs='        "$META_FILE" | cut -d= -f2-)
    ipv6=$(grep      '^ipv6='        "$META_FILE" | cut -d= -f2-)
    tfo=$(grep       '^tfo='         "$META_FILE" | cut -d= -f2-)
    mode=$(grep      '^mode='        "$META_FILE" | cut -d= -f2-)
    dns_pref=$(grep  '^dns_pref='    "$META_FILE" | cut -d= -f2-)
    node_name=$(grep '^node_name='   "$META_FILE" | cut -d= -f2-)
    geo_ip=$(grep    '^geo_ip='      "$META_FILE" | cut -d= -f2-)
    # Installs made before v6 support never wrote a proto line; they are
    # all protocol v5.
    proto="${proto:-5}"

    # Prefer the IP we resolved at install time; if that wasn't captured
    # (e.g. ip.sb was down then), try again now, and last-ditch fall back
    # to a guaranteed-non-empty placeholder.
    ip="${geo_ip:-}"
    if [ -z "$ip" ]; then
        ip=$(get_ipv4)
    fi
    [ -z "$ip" ] && ip="YOUR_SERVER_IP"

    # Node name should always be present (build_config writes one even
    # when ip.sb fails). Regenerate on the fly if somehow missing.
    if [ -z "$node_name" ]; then
        fetch_geo || true
        node_name=$(generate_node_name)
    fi

    print_header "Connection Info"
    echo -e "${BOLD}Node name:${NC}    ${node_name}"
    echo -e "${BOLD}Variant:${NC}      ${variant} (${version})"
    echo -e "${BOLD}Server IP:${NC}    ${ip}"
    echo -e "${BOLD}Protocol:${NC}     v${proto}"
    echo -e "${BOLD}Port:${NC}         ${port}"
    echo -e "${BOLD}PSK:${NC}          ${psk}"
    if [ "$proto" = "6" ]; then
        echo -e "${BOLD}Mode:${NC}         ${mode:-default}"
        echo -e "${BOLD}DNS pref:${NC}     ${dns_pref:-default}"
        echo -e "${BOLD}TFO:${NC}          ${tfo}"
    else
        echo -e "${BOLD}obfs:${NC}         ${obfs}"
        echo -e "${BOLD}IPv6 egress:${NC}  ${ipv6}"
        echo -e "${BOLD}TFO:${NC}          ${tfo}"
    fi

    # Surge proxy line. We deliberately omit `block-quic` (it's `auto`
    # by default on the Surge side, which is what we want) and only
    # advertise `tfo=true` when this server actually negotiated it.
    print_header "Surge proxy line (copy into your Surge [Proxy] section)"
    local tfo_param=""
    [ "$tfo" = "true" ] && tfo_param=", tfo=true"
    echo -e "${GREEN}${node_name} = snell, ${ip}, ${port}, psk=\"${psk}\", version=${proto}${tfo_param}${NC}"

    print_header "Mihomo proxy (copy into proxies)"
    local mihomo_name mihomo_server mihomo_psk mihomo_tfo=""
    mihomo_name=$(yaml_double_quote_escape "$node_name")
    mihomo_server=$(yaml_double_quote_escape "$ip")
    mihomo_psk=$(yaml_double_quote_escape "$psk")
    [ "$tfo" = "true" ] && mihomo_tfo=", tfo: true"
    printf '%b- {name: "%s", server: "%s", port: %s, type: snell, psk: "%s", version: %s%s}%b\n' \
        "$GREEN" "$mihomo_name" "$mihomo_server" "$port" "$mihomo_psk" "$proto" "$mihomo_tfo" "$NC"

    print_header "Shadowrocket URL"
    local shadowrocket_payload shadowrocket_name shadowrocket_tfo
    shadowrocket_payload=$(base64_encode "chacha20-ietf-poly1305:${psk}@${ip}:${port}")
    shadowrocket_name=$(url_encode "$node_name")
    if [ "$tfo" = "true" ]; then shadowrocket_tfo="1"; else shadowrocket_tfo="0"; fi
    printf '%bsnell://%s?tfo=%s&version=%s#%s%b\n' \
        "$GREEN" "$shadowrocket_payload" "$shadowrocket_tfo" "$proto" "$shadowrocket_name" "$NC"

    # v6's `mode` changes the wire format, so a non-default server mode only
    # works with a client set to the same mode. The lines above carry no mode
    # token (clients infer `default`); flag the mismatch instead of silently
    # printing a config that won't connect.
    if [ "$proto" = "6" ] && [ -n "$mode" ] && [ "$mode" != "default" ]; then
        print_warning "Server mode is '${mode}', not 'default' — set the SAME mode on the client,"
        print_warning "or the handshake will fail. The proxy lines above assume the client's mode matches."
    fi

    print_header "Service"
    systemctl is-active --quiet "$SERVICE_NAME" \
        && print_success "snell-server is running" \
        || print_warning "snell-server is NOT running (try: systemctl start $SERVICE_NAME)"
}

# ============================================================================
# Top-level actions
# ============================================================================
do_install() {
    check_root; ensure_tools

    print_header "OpenSnell server installer"
    echo -e "${BOLD}Choose a variant:${NC}"
    echo -e "${GREEN}1)${NC} OpenSnell ${YELLOW}(default, GPLv3, all-platform)${NC}"
    echo -e "${GREEN}2)${NC} Surge official snell-server ${SURGE_V5_VERSION} ${YELLOW}(closed-source, Linux only)${NC}"
    echo -e "${GREEN}3)${NC} Surge official snell-server ${SURGE_V6_VERSION} ${YELLOW}(closed-source beta, Linux only)${NC}"
    echo
    read -r -p "$(echo -e "${CYAN}Variant [${BOLD}1${NC}${CYAN}]: ${NC}")" variant_choice
    case "${variant_choice:-1}" in
        1) download_opensnell ;;
        2) download_surge "$SURGE_V5_VERSION" ;;
        3) note_v6_beta; download_surge "$SURGE_V6_VERSION" ;;
        *) print_error "Invalid choice"; exit 1 ;;
    esac

    build_config
    write_systemd_unit
    configure_firewall
    enable_service
    systemctl restart "$SERVICE_NAME"
    sleep 1
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        print_success "Service is running"
    else
        print_warning "Service is NOT running yet — check 'journalctl -u $SERVICE_NAME -n 30'"
    fi
    show_info
}

do_reconfigure() {
    check_root
    [ -f "$INSTALL_BIN" ] || { print_error "Binary not found; install first."; exit 1; }
    build_config
    write_systemd_unit
    configure_firewall
    restart_service
    show_info
}

do_update() {
    check_root; ensure_tools
    local variant; variant=$(get_install_variant)
    [ -z "$variant" ] && variant="opensnell"
    print_info "Updating variant: $variant"
    if [ "$variant" = "surge" ]; then
        # Stay on the installed major: a v6 install updates within v6,
        # everything else (including pre-v6-support meta) stays on v5.
        case "$(get_installed_version)" in
            v6*) download_surge "$SURGE_V6_VERSION" ;;
            *)   download_surge "$SURGE_V5_VERSION" ;;
        esac
    else
        download_opensnell
    fi
    # Merge variant/version from .tmp into existing meta
    if [ -f "$META_FILE.tmp" ]; then
        local newver; newver=$(grep '^version=' "$META_FILE.tmp" | cut -d= -f2)
        sed -i.bak "s/^version=.*/version=${newver}/" "$META_FILE" 2>/dev/null || true
        rm -f "$META_FILE.tmp" "$META_FILE.bak"
    fi
    restart_service
    print_success "Update complete"
}

do_uninstall() {
    check_root
    print_warning "This will stop the service and remove the binary."
    local rm_cfg confirm
    confirm=$(prompt_yesno "Continue?" "n")
    [ "$confirm" != "y" ] && { print_info "Aborted."; return; }
    systemctl stop    "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    rm -f "$INSTALL_BIN"
    print_success "Removed binary and service unit"
    rm_cfg=$(prompt_yesno "Also remove $CONFIG_DIR (configuration + PSK)?" "n")
    if [ "$rm_cfg" = "y" ]; then
        rm -rf "$CONFIG_DIR"
        print_success "Removed $CONFIG_DIR"
    else
        print_info "Configuration kept at $CONFIG_DIR"
    fi
}

# ============================================================================
# CLI
# ============================================================================
show_help() {
    cat <<EOF
OpenSnell server installer

Usage: $0 <subcommand>

Subcommands:
  install        Interactive install (default action when run without args)
  reconfigure    Rewrite the snell-server.conf interactively, restart service
  update         Re-download the binary, restart service
  uninstall      Stop service + remove binary (+ optionally remove config)
  start | stop | restart | enable | disable | status
  info           Print server IP / port / PSK / Surge config snippet
  help           Show this help

Interactive menu is shown when no subcommand is given.
EOF
}

show_menu() {
    echo -e "${BOLD}${MAGENTA}=====================================================${NC}"
    echo -e "${BOLD}${MAGENTA}        OpenSnell Server Management Script           ${NC}"
    echo -e "${BOLD}${MAGENTA}=====================================================${NC}"
    echo
    echo -e "${GREEN}1)${NC}  Install"
    echo -e "${GREEN}2)${NC}  Reconfigure"
    echo -e "${GREEN}3)${NC}  Update binary"
    echo -e "${RED}4)${NC}  Uninstall"
    echo -e "${BLUE}5)${NC}  Start"
    echo -e "${BLUE}6)${NC}  Stop"
    echo -e "${BLUE}7)${NC}  Restart"
    echo -e "${CYAN}8)${NC}  Enable auto-start"
    echo -e "${CYAN}9)${NC}  Disable auto-start"
    echo -e "${YELLOW}10)${NC} Show status"
    echo -e "${YELLOW}11)${NC} Show connection info"
    echo -e "${MAGENTA}0)${NC}  Exit"
    echo
    read -r -p "$(echo -e "${CYAN}Enter your choice (0-11): ${NC}")" choice
    case "$choice" in
        1)  do_install        ;;
        2)  do_reconfigure    ;;
        3)  do_update         ;;
        4)  do_uninstall      ;;
        5)  start_service     ;;
        6)  stop_service      ;;
        7)  restart_service   ;;
        8)  enable_service    ;;
        9)  disable_service   ;;
        10) status_service    ;;
        11) show_info         ;;
        0)  print_info "Bye."; exit 0 ;;
        *)  print_error "Invalid option"; exit 1 ;;
    esac
}

main() {
    # `help` is the only subcommand that should work anywhere; everything
    # else needs systemd / iptables / sysctl etc., so refuse non-Linux
    # immediately rather than fail halfway through.
    case "${1:-}" in
        help|--help|-h) show_help; return ;;
    esac
    check_linux

    case "${1:-}" in
        install)        do_install        ;;
        reconfigure)    do_reconfigure    ;;
        update|upgrade) do_update         ;;
        uninstall)      do_uninstall      ;;
        start)          check_root; start_service     ;;
        stop)           check_root; stop_service      ;;
        restart)        check_root; restart_service   ;;
        enable)         check_root; enable_service    ;;
        disable)        check_root; disable_service   ;;
        status)         status_service                ;;
        info)           show_info                     ;;
        "")             show_menu                     ;;
        *)              print_error "Unknown command: $1"; show_help; exit 1 ;;
    esac
}

main "$@"
