#!/usr/bin/env bash
# Simorgh - WireGuard protocol module.
# Sourced by install.sh. Provides:
#   wg_install_core          - install wireguard-tools, bring up wg0
#   wg_add_customer <name>   - create a WireGuard peer + matching Simorgh
#                              carrier customer, print what to hand out
#
# This installs and configures the standard, upstream wireguard-tools
# package (wg / wg-quick) - it does not reimplement WireGuard. If you're
# reading this because something broke, see the troubleshooting table in
# docs/DEPLOYMENT.md before assuming the script itself is wrong; most
# WireGuard failures are environment issues (kernel module, firewall,
# ip_forward) rather than config-generation bugs.

WG_DIR="/etc/wireguard"
WG_CONF="$WG_DIR/wg0.conf"
WG_IFACE="wg0"
WG_PORT_DEFAULT="51820"
WG_SUBNET_DEFAULT="10.66.66.1/24"
SIMORGH_CUSTOMERS_FILE="/etc/simorgh/customers.json"

_wg_log()  { echo -e "  ${Y:-}$*${NC:-}"; }
_wg_ok()   { echo -e "  ${G:-}[OK]${NC:-} $*"; }
_wg_err()  { echo -e "  ${R:-}[ERROR]${NC:-} $*"; }

# ---------------------------------------------------------------------
# OS detection - branch package manager / package name accordingly.
# Only Debian/Ubuntu and RHEL-family are handled; anything else fails
# loudly with a clear message rather than guessing.
# ---------------------------------------------------------------------
_wg_detect_os() {
    if [ -f /etc/debian_version ]; then
        echo "debian"
    elif [ -f /etc/redhat-release ]; then
        echo "rhel"
    else
        echo "unknown"
    fi
}

_wg_install_package() {
    local os; os="$(_wg_detect_os)"
    case "$os" in
        debian)
            apt-get update -q >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            apt-get install -y -q wireguard-tools >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            ;;
        rhel)
            # WireGuard is in EPEL on older RHEL/CentOS; on 9+ it's often
            # already in the base repos as kmod-wireguard-tools -
            # try the common name and fall back to epel-release first.
            (yum install -y wireguard-tools >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1) || \
            (yum install -y epel-release >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1 && \
             yum install -y wireguard-tools >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1)
            ;;
        *)
            _wg_err "Unsupported OS - only Debian/Ubuntu and RHEL-family are automated."
            _wg_log "Install wireguard-tools manually for your distro, then re-run this."
            return 1
            ;;
    esac
}

# ---------------------------------------------------------------------
# Core install: package, keys, wg0.conf, forwarding, service.
# Idempotent - safe to re-run (won't regenerate keys or clobber peers
# already in wg0.conf).
# ---------------------------------------------------------------------
wg_install_core() {
    _wg_log "Installing wireguard-tools..."
    if ! command -v wg &>/dev/null; then
        if ! _wg_install_package; then
            return 1
        fi
    fi
    if ! command -v wg &>/dev/null; then
        _wg_err "wg command still not found after install - check ${LOG_FILE:-/tmp/simorgh_install.log}"
        return 1
    fi
    _wg_ok "wireguard-tools installed."

    mkdir -p "$WG_DIR"
    chmod 700 "$WG_DIR"

    if [ ! -f "$WG_DIR/server_private.key" ]; then
        _wg_log "Generating server keypair..."
        umask 077
        wg genkey | tee "$WG_DIR/server_private.key" | wg pubkey > "$WG_DIR/server_public.key"
        _wg_ok "Keypair generated."
    else
        _wg_log "Server keypair already exists, keeping it."
    fi

    local listen_port server_subnet ext_iface
    listen_port="${WG_LISTEN_PORT:-}"
    if [ -z "$listen_port" ] && [ -t 0 ]; then
        read -r -p "  WireGuard listen port [$WG_PORT_DEFAULT]: " listen_port
    fi
    listen_port="${listen_port:-$WG_PORT_DEFAULT}"

    server_subnet="${WG_SERVER_SUBNET:-}"
    if [ -z "$server_subnet" ] && [ -t 0 ]; then
        read -r -p "  Server VPN subnet [$WG_SUBNET_DEFAULT]: " server_subnet
    fi
    server_subnet="${server_subnet:-$WG_SUBNET_DEFAULT}"

    ext_iface="${WG_EXT_IFACE:-}"
    if [ -z "$ext_iface" ]; then
        ext_iface="$(ip route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
    fi
    if [ -z "$ext_iface" ] && [ -t 0 ]; then
        _wg_err "Could not auto-detect the default network interface for NAT."
        read -r -p "  Enter it manually (e.g. eth0): " ext_iface
    fi

    if [ ! -f "$WG_CONF" ]; then
        local server_priv; server_priv="$(cat "$WG_DIR/server_private.key")"
        cat > "$WG_CONF" <<EOF
[Interface]
Address = $server_subnet
ListenPort = $listen_port
PrivateKey = $server_priv
PostUp = iptables -t nat -A POSTROUTING -o $ext_iface -j MASQUERADE; iptables -A FORWARD -i %i -j ACCEPT; iptables -A FORWARD -o %i -j ACCEPT
PostDown = iptables -t nat -D POSTROUTING -o $ext_iface -j MASQUERADE; iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT

EOF
        chmod 600 "$WG_CONF"
        _wg_ok "wg0.conf created (port $listen_port, subnet $server_subnet, NAT via $ext_iface)."
    else
        _wg_log "$WG_CONF already exists, leaving it as-is (peers, if any, are preserved)."
    fi

    _wg_log "Enabling IP forwarding..."
    sysctl -w net.ipv4.ip_forward=1 >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
    if ! grep -q '^net.ipv4.ip_forward' /etc/sysctl.conf 2>/dev/null; then
        echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
    else
        sed -i 's/^net.ipv4.ip_forward.*/net.ipv4.ip_forward=1/' /etc/sysctl.conf
    fi

    _wg_log "Starting wg-quick@$WG_IFACE..."
    systemctl enable --now "wg-quick@$WG_IFACE" >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1

    sleep 1
    if systemctl is-active --quiet "wg-quick@$WG_IFACE" && wg show "$WG_IFACE" &>/dev/null; then
        _wg_ok "WireGuard is up. \`wg show $WG_IFACE\` confirms the interface exists."
    else
        _wg_err "wg-quick@$WG_IFACE did not come up cleanly."
        _wg_log "Run: systemctl status wg-quick@$WG_IFACE"
        _wg_log "And: journalctl -u wg-quick@$WG_IFACE -n 50 --no-pager"
        _wg_log "See docs/DEPLOYMENT.md -> WireGuard troubleshooting for common causes"
        _wg_log "(kernel module missing, port already in use, malformed wg0.conf)."
        return 1
    fi
}

# ---------------------------------------------------------------------
# Add one customer: a WireGuard peer + a matching Simorgh carrier
# customer entry, both keyed to the same human-readable name.
# ---------------------------------------------------------------------
wg_add_customer() {
    local name="$1"
    local bw_arg="${2:-}"
    if [ -z "$name" ]; then
        read -r -p "  Customer name: " name
    fi
    if [ -z "$name" ]; then
        _wg_err "Name cannot be empty."
        return 1
    fi
    if grep -q "# customer:$name\$" "$WG_CONF" 2>/dev/null; then
        _wg_err "A peer for \"$name\" already exists in $WG_CONF."
        return 1
    fi

    if [ ! -f "$WG_CONF" ]; then
        _wg_err "wg0.conf not found - run 'Install WireGuard Core' first."
        return 1
    fi

    local client_priv client_pub server_pub server_subnet listen_port
    client_priv="$(wg genkey)"
    client_pub="$(echo "$client_priv" | wg pubkey)"
    server_pub="$(cat "$WG_DIR/server_public.key")"
    server_subnet="$(grep -m1 '^Address' "$WG_CONF" | cut -d= -f2 | tr -d ' ')"
    listen_port="$(grep -m1 '^ListenPort' "$WG_CONF" | cut -d= -f2 | tr -d ' ')"

    # next free host in the subnet: count existing peers, offset from .2
    local net_base peer_count client_ip
    net_base="$(echo "$server_subnet" | sed -E 's#\.[0-9]+/.*##')"
    peer_count="$(grep -c '^\[Peer\]' "$WG_CONF" 2>/dev/null)"
    peer_count="${peer_count:-0}"
    client_ip="${net_base}.$((peer_count + 2))"

    cat >> "$WG_CONF" <<EOF
[Peer] # customer:$name
PublicKey = $client_pub
AllowedIPs = $client_ip/32

EOF

    if wg syncconf "$WG_IFACE" <(wg-quick strip "$WG_IFACE") 2>>"${LOG_FILE:-/tmp/simorgh_install.log}"; then
        _wg_ok "Peer added and applied live (no restart, existing customers unaffected)."
    else
        _wg_err "wg syncconf failed - the peer is saved in $WG_CONF but not yet live."
        _wg_log "Restart manually: systemctl restart wg-quick@$WG_IFACE"
    fi

    # bw_limit resolution order: explicit 2nd arg (scripted/remote calls) ->
    # interactive prompt (only if attached to a real terminal) -> unlimited.
    local bw_limit="$bw_arg"
    if [ -z "$bw_limit" ] && [ -t 0 ]; then
        read -r -p "  Bandwidth cap for this customer in Mbps (empty = unlimited): " bw_limit
    fi

    echo
    _wg_ok "Customer \"$name\" created."
    echo

    if [ -n "${WG_DIRECT_HOST:-}" ]; then
        # Plain multi-location WireGuard: the client connects straight to
        # this node's real public IP. No Simorgh carrier involved - this is
        # the right choice when you just want "N independent locations",
        # the same shape as picking a country in Marzban/other multi-node
        # panels, just without needing a central orchestrator to get there.
        echo "  ── WireGuard client config (wg-$name.conf) ──"
        cat <<EOF
[Interface]
PrivateKey = $client_priv
Address = $client_ip/32
DNS = 1.1.1.1

[Peer]
PublicKey = $server_pub
Endpoint = ${WG_DIRECT_HOST}:$listen_port
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
EOF
        echo
        _wg_log "Direct connection to this node - no Simorgh carrier, no extra"
        _wg_log "software needed on the customer's device beyond WireGuard itself."
        return 0
    fi

    # Carrier mode: pair this WireGuard peer with an isolated Simorgh
    # carrier customer for lower ping / packet-loss recovery on the hop to
    # this node.
    local simorgh_pw local_port
    simorgh_pw="$(head -c24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c24)"
    local_port="$((51000 + peer_count + 1))"
    _wg_add_simorgh_customer "$name" "$simorgh_pw" "$bw_limit"

    echo "  ── 1) WireGuard client config (wg-$name.conf) ──"
    cat <<EOF
[Interface]
PrivateKey = $client_priv
Address = $client_ip/32
DNS = 1.1.1.1

[Peer]
PublicKey = $server_pub
Endpoint = 127.0.0.1:$local_port
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
EOF
    echo
    echo "  ── 2) Their Simorgh carrier client (run on their own device) ──"
    echo "  docker run -d --name simorgh_client --cap-add=NET_ADMIN --cap-add=NET_RAW \\"
    echo "    --device /dev/net/tun:/dev/net/tun --net=host --restart unless-stopped \\"
    echo "    -e MODE=forward -e TRANSPORT=icmp -e FORWARD_PROTO=udp \\"
    echo "    -e REMOTE_IP=<THIS_SERVER_PUBLIC_IP> -e FORWARD_BIND=127.0.0.1 -e LOCAL_PORT=$local_port \\"
    echo "    -e PASSWORD=$simorgh_pw simorgh-core:latest"
    echo
    if [ -n "$bw_limit" ]; then
        _wg_log "Bandwidth: capped at ${bw_limit} Mbps by Simorgh's carrier hop - this is"
        _wg_log "enforced by Simorgh itself (a real token-bucket limiter), since neither"
        _wg_log "WireGuard nor 3X-UI/vpn-ui-family panels have a built-in equivalent as of"
        _wg_log "this writing (see docs/DEPLOYMENT.md)."
    fi
    _wg_log "Note: the customer needs BOTH pieces running - the Simorgh carrier client"
    _wg_log "establishes the low-latency tunnel first, then WireGuard connects through"
    _wg_log "it locally. If you'd rather they connect straight to WireGuard with no"
    _wg_log "carrier, just give them the .conf with Endpoint = <THIS_SERVER_PUBLIC_IP>:$listen_port instead"
    _wg_log "(note: skipping the carrier also means no Simorgh-side bandwidth cap)."
}

_wg_add_simorgh_customer() {
    local name="$1" password="$2" bw_limit="${3:-}"
    mkdir -p "$(dirname "$SIMORGH_CUSTOMERS_FILE")"
    if [ ! -f "$SIMORGH_CUSTOMERS_FILE" ]; then
        echo "[]" > "$SIMORGH_CUSTOMERS_FILE"
    fi
    local tmp; tmp="$(mktemp)"
    python3 - "$SIMORGH_CUSTOMERS_FILE" "$name" "$password" "$bw_limit" "$tmp" <<'PYEOF'
import json, sys
path, name, password, bw_limit, tmp = sys.argv[1:6]
with open(path) as f:
    data = json.load(f)
data = [c for c in data if c.get("name") != name]
entry = {"name": name, "password": password}
if bw_limit.strip():
    try:
        entry["bandwidth_mbps"] = float(bw_limit)
    except ValueError:
        pass
data.append(entry)
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
PYEOF
    mv "$tmp" "$SIMORGH_CUSTOMERS_FILE"
    chmod 600 "$SIMORGH_CUSTOMERS_FILE"
}

wg_list_customers() {
    if [ ! -f "$WG_CONF" ]; then
        _wg_err "No wg0.conf yet."
        return
    fi
    grep '^\[Peer\] # customer:' "$WG_CONF" | sed -E 's/.*customer:(.*)/  - \1/'
}

wg_remove_customer() {
    local name="$1"
    if [ -z "$name" ]; then
        read -r -p "  Customer name to remove: " name
    fi
    if [ ! -f "$WG_CONF" ] || ! grep -q "# customer:$name\$" "$WG_CONF"; then
        _wg_err "No such customer in $WG_CONF."
        return 1
    fi
    # remove the [Peer] block for this customer (3 lines: header, PublicKey, AllowedIPs, blank)
    python3 - "$WG_CONF" "$name" <<'PYEOF'
import sys
path, name = sys.argv[1], sys.argv[2]
with open(path) as f:
    lines = f.readlines()
out = []
skip = False
for line in lines:
    if line.strip() == f"[Peer] # customer:{name}":
        skip = True
        continue
    if skip and line.strip() == "":
        skip = False
        continue
    if not skip:
        out.append(line)
with open(path, "w") as f:
    f.writelines(out)
PYEOF
    wg syncconf "$WG_IFACE" <(wg-quick strip "$WG_IFACE") 2>>"${LOG_FILE:-/tmp/simorgh_install.log}" || true

    if [ -f "$SIMORGH_CUSTOMERS_FILE" ]; then
        local tmp; tmp="$(mktemp)"
        python3 - "$SIMORGH_CUSTOMERS_FILE" "$name" "$tmp" <<'PYEOF'
import json, sys
path, name, tmp = sys.argv[1:4]
with open(path) as f:
    data = json.load(f)
data = [c for c in data if c.get("name") != name]
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
PYEOF
        mv "$tmp" "$SIMORGH_CUSTOMERS_FILE"
    fi
    _wg_ok "Customer \"$name\" removed from both WireGuard and Simorgh."
}
