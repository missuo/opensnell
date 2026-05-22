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
# Two install variants:
#   1) OpenSnell (default, GPLv3, all-platform, this repo)
#   2) Surge official snell-server v5.0.1 (closed-source, Linux only)
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
# CHANNEL = stable | alpha. Stable is the default; alpha pulls from the
# rolling `alpha` tag that the alpha-branch CI publishes on every push.
# The channel is set from a top-level `--alpha` script flag (parsed in
# main) and persisted to the install meta file so subsequent `update`
# runs stay on the same channel without needing the flag again.
CHANNEL="stable"
OPENSNELL_RELEASE_API_STABLE="https://api.github.com/repos/${OPENSNELL_REPO}/releases/latest"
OPENSNELL_RELEASE_API_ALPHA="https://api.github.com/repos/${OPENSNELL_REPO}/releases/tags/alpha"
SURGE_VERSION="v5.0.1"
SURGE_BASE_URL="https://dl.nssurge.com/snell"

opensnell_release_api() {
    case "$CHANNEL" in
        alpha) echo "$OPENSNELL_RELEASE_API_ALPHA" ;;
        *)     echo "$OPENSNELL_RELEASE_API_STABLE" ;;
    esac
}

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
# If the running kernel already reports 3, we DON'T touch the user's
# system — we only write `tfo = true` to opensnell.conf and trust the
# kernel. We only edit /etc/sysctl.conf when the kernel is missing one
# of the bits, and even then we keep the edit minimal (sed-or-append).
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
        print_warning "Skipping; OpenSnell will still set the socket option, but TFO won't actually take effect"
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

get_install_channel() {
    [ -f "$META_FILE" ] && grep '^channel=' "$META_FILE" | cut -d= -f2 || true
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
    local arch tag url api channel_label
    arch=$(detect_arch_opensnell)
    api=$(opensnell_release_api)
    tag=$(curl -fsSL "$api" | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
    if [ -z "$tag" ]; then
        if [ "$CHANNEL" = "alpha" ]; then
            print_error "Could not resolve the rolling 'alpha' release from GitHub API."
            print_info "The alpha branch CI may not have produced an alpha release yet — check:"
            print_info "    https://github.com/${OPENSNELL_REPO}/releases/tag/alpha"
        else
            print_error "Could not resolve latest release from GitHub API."
        fi
        print_info "Build from source instead: go install github.com/${OPENSNELL_REPO}/cmd/snell-server@latest"
        exit 1
    fi
    # The release publishes raw, per-target binaries — no archive wrapper.
    # Linux assets are unsuffixed; Windows ones end in .exe but we don't
    # consume those here (this installer is Linux-only by design).
    url="https://github.com/${OPENSNELL_REPO}/releases/download/${tag}/snell-server-linux-${arch}"

    if [ "$CHANNEL" = "alpha" ]; then
        channel_label="OpenSnell (alpha channel — rolling pre-release)"
    else
        channel_label="OpenSnell (self-hosted, GPLv3)"
    fi
    print_info "Variant:      ${channel_label}"
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
    echo "channel=$CHANNEL"  >> "$META_FILE.tmp"
}

download_surge() {
    print_header "Downloading Surge official snell-server"
    mkdir -p "$CONFIG_DIR"
    local arch url workdir
    arch=$(detect_arch_surge)
    url="${SURGE_BASE_URL}/snell-server-${SURGE_VERSION}-linux-${arch}.zip"

    print_info "Variant:      Surge official (closed-source, Linux only)"
    print_info "Architecture: linux/${arch}"
    print_info "Version:      ${SURGE_VERSION}"
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
    print_success "Installed Surge snell-server ${SURGE_VERSION} → ${INSTALL_BIN}"

    echo "variant=surge"             >  "$META_FILE.tmp"
    echo "version=${SURGE_VERSION}"  >> "$META_FILE.tmp"
}

# ============================================================================
# Interactive config builder
# ============================================================================
build_config() {
    # Prefer the variant set during the immediately-preceding download (we
    # haven't committed META_FILE yet), or fall back to existing meta on
    # reconfigure.
    local variant=""
    if [ -f "$META_FILE.tmp" ]; then
        variant=$(grep '^variant=' "$META_FILE.tmp" | cut -d= -f2)
    elif [ -f "$META_FILE" ]; then
        variant=$(grep '^variant=' "$META_FILE" | cut -d= -f2)
    fi
    variant="${variant:-opensnell}"

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

    # --- obfs ---
    local obfs
    obfs=$(prompt_default "obfs mode (off/http/tls)" "off")
    case "$obfs" in off|http|tls) ;; *) print_error "obfs must be one of: off, http, tls"; exit 1 ;; esac

    # --- ipv6 ---
    local ipv6_choice ipv6
    ipv6_choice=$(prompt_yesno "Allow IPv6 destinations (server-side outbound)" "y")
    if [ "$ipv6_choice" = "y" ]; then ipv6="true"; else ipv6="false"; fi

    # OpenSnell-only knobs
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
    fi

    # --- Write config ---
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

    # Surge has its own per-proxy tfo=true, which our snell-server happily
    # ignores when running as the Surge variant. So TFO config is OpenSnell-only.
    if [ "$variant" = "opensnell" ] && [ "$tfo" = "true" ]; then
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
        grep -vE '^(port|psk|obfs|ipv6|tfo|node_name|geo_ip|geo_country)=' "$META_FILE" 2>/dev/null || true
        echo "port=$port"
        echo "psk=$psk"
        echo "obfs=$obfs"
        echo "ipv6=$ipv6"
        echo "tfo=$tfo"
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
    local variant version channel port psk obfs ipv6 tfo node_name geo_ip ip
    variant=$(grep   '^variant='     "$META_FILE" | cut -d= -f2-)
    version=$(grep   '^version='     "$META_FILE" | cut -d= -f2-)
    channel=$(grep   '^channel='     "$META_FILE" | cut -d= -f2-)
    port=$(grep      '^port='        "$META_FILE" | cut -d= -f2-)
    psk=$(grep       '^psk='         "$META_FILE" | cut -d= -f2-)
    obfs=$(grep      '^obfs='        "$META_FILE" | cut -d= -f2-)
    ipv6=$(grep      '^ipv6='        "$META_FILE" | cut -d= -f2-)
    tfo=$(grep       '^tfo='         "$META_FILE" | cut -d= -f2-)
    node_name=$(grep '^node_name='   "$META_FILE" | cut -d= -f2-)
    geo_ip=$(grep    '^geo_ip='      "$META_FILE" | cut -d= -f2-)
    [ -z "$channel" ] && channel="stable"

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
    if [ "$variant" = "opensnell" ]; then
        echo -e "${BOLD}Channel:${NC}      ${channel}"
    fi
    echo -e "${BOLD}Server IP:${NC}    ${ip}"
    echo -e "${BOLD}Port:${NC}         ${port}"
    echo -e "${BOLD}PSK:${NC}          ${psk}"
    echo -e "${BOLD}obfs:${NC}         ${obfs}"
    echo -e "${BOLD}IPv6 egress:${NC}  ${ipv6}"
    echo -e "${BOLD}TFO:${NC}          ${tfo}"

    # Surge proxy line. We deliberately omit `block-quic` (it's `auto`
    # by default on the Surge side, which is what we want) and only
    # advertise `tfo=true` when this server actually negotiated it.
    print_header "Surge proxy line (copy into your Surge [Proxy] section)"
    local tfo_param=""
    [ "$tfo" = "true" ] && tfo_param=", tfo=true"
    echo -e "${GREEN}${node_name} = snell, ${ip}, ${port}, psk=\"${psk}\", version=5${tfo_param}${NC}"

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
    echo -e "${GREEN}2)${NC} Surge official snell-server v5.0.1 ${YELLOW}(closed-source, Linux only)${NC}"
    echo
    read -r -p "$(echo -e "${CYAN}Variant [${BOLD}1${NC}${CYAN}]: ${NC}")" variant_choice
    case "${variant_choice:-1}" in
        1) download_opensnell ;;
        2) download_surge     ;;
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

    # Honor the previously-installed channel unless the user explicitly
    # passed --alpha on this invocation. So `update` after `install --alpha`
    # keeps fetching alpha without needing the flag again.
    if [ "$CHANNEL" = "stable" ]; then
        local persisted_channel
        persisted_channel=$(get_install_channel)
        if [ -n "$persisted_channel" ]; then
            CHANNEL="$persisted_channel"
        fi
    fi

    print_info "Updating variant: $variant (channel: $CHANNEL)"
    if [ "$variant" = "surge" ]; then
        download_surge
    else
        download_opensnell
    fi
    # Merge variant/version/channel from .tmp into existing meta. We
    # rewrite version + channel in place; variant is already correct.
    if [ -f "$META_FILE.tmp" ]; then
        local newver newchan
        newver=$(grep '^version=' "$META_FILE.tmp" | cut -d= -f2)
        newchan=$(grep '^channel=' "$META_FILE.tmp" | cut -d= -f2)
        sed -i.bak "s/^version=.*/version=${newver}/" "$META_FILE" 2>/dev/null || true
        if [ -n "$newchan" ]; then
            if grep -q '^channel=' "$META_FILE" 2>/dev/null; then
                sed -i.bak "s/^channel=.*/channel=${newchan}/" "$META_FILE"
            else
                echo "channel=${newchan}" >> "$META_FILE"
            fi
        fi
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

Usage: $0 [--alpha] <subcommand>

Subcommands:
  install        Interactive install (default action when run without args)
  reconfigure    Rewrite the snell-server.conf interactively, restart service
  update         Re-download the binary, restart service
  uninstall      Stop service + remove binary (+ optionally remove config)
  start | stop | restart | enable | disable | status
  info           Print server IP / port / PSK / Surge config snippet
  help           Show this help

Channel flags (apply to install / update of the OpenSnell variant only;
the Surge variant has only one official build):

  --alpha        Pull from the rolling "alpha" GitHub release instead of
                 the latest stable tag. Useful for trying experimental
                 features ahead of release. The channel is persisted to
                 /etc/snell/.install_meta so future updates stay on the
                 same channel without re-passing the flag. To leave the
                 alpha channel, run \`install\` (without --alpha) again.

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
    # Parse global flags (currently only --alpha). They can appear in any
    # order relative to the subcommand. We rebuild positional args without
    # the consumed flag(s) so the subcommand dispatcher below sees a clean
    # arg list.
    local positional=()
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --alpha)        CHANNEL="alpha";    shift ;;
            --stable)       CHANNEL="stable";   shift ;;
            help|--help|-h) show_help; return ;;
            *)              positional+=("$1"); shift ;;
        esac
    done
    set -- "${positional[@]+"${positional[@]}"}"

    # `help` is the only subcommand that should work anywhere; everything
    # else needs systemd / iptables / sysctl etc., so refuse non-Linux
    # immediately rather than fail halfway through.
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
