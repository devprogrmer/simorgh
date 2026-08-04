#!/usr/bin/env bash
# Simorgh - OpenVPN protocol module.
# Sourced by install.sh / invoked remotely by nodepanel. Provides:
#   ovpn_install_core                 - install openvpn+easy-rsa, set up PKI, bring up server
#   ovpn_add_customer <name>          - create a client cert + .ovpn on THIS node
#   ovpn_export_ca / ovpn_import_ca   - share one CA across multiple nodes, so one
#                                        client cert is valid on all of them (this is
#                                        what makes a single multi-remote .ovpn possible)
#
# Installs and configures the standard, upstream `openvpn` + `easy-rsa` packages -
# does not reimplement the protocol. See docs/DEPLOYMENT.md for prerequisites and
# troubleshooting.

OVPN_DIR="/etc/openvpn/server"
OVPN_PKI_DIR="$OVPN_DIR/easy-rsa"
OVPN_CONF="$OVPN_DIR/server.conf"
OVPN_PORT_DEFAULT="1194"
OVPN_SUBNET_DEFAULT="10.88.88.0 255.255.255.0"
EASYRSA_BIN="/usr/share/easy-rsa/easyrsa"

_ovpn_log() { echo -e "  ${Y:-}$*${NC:-}"; }
_ovpn_ok()  { echo -e "  ${G:-}[OK]${NC:-} $*"; }
_ovpn_err() { echo -e "  ${R:-}[ERROR]${NC:-} $*"; }

_ovpn_detect_os() {
    if [ -f /etc/debian_version ]; then echo "debian"
    elif [ -f /etc/redhat-release ]; then echo "rhel"
    else echo "unknown"
    fi
}

_ovpn_install_package() {
    local os; os="$(_ovpn_detect_os)"
    case "$os" in
        debian)
            apt-get update -q >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            apt-get install -y -q openvpn easy-rsa >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            ;;
        rhel)
            (yum install -y openvpn easy-rsa >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1) || \
            (yum install -y epel-release >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1 && \
             yum install -y openvpn easy-rsa >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1)
            ;;
        *)
            _ovpn_err "Unsupported OS - only Debian/Ubuntu and RHEL-family are automated."
            return 1
            ;;
    esac
}

_ovpn_find_easyrsa() {
    if command -v easyrsa &>/dev/null; then command -v easyrsa; return; fi
    if [ -x "$EASYRSA_BIN" ]; then echo "$EASYRSA_BIN"; return; fi
    find /usr/share -maxdepth 2 -iname "easyrsa" -type f 2>/dev/null | head -1
}

# ---------------------------------------------------------------------
# Core install: package, PKI (fresh or shared/imported), server.conf,
# forwarding, service. Idempotent - safe to re-run.
#
# Env vars (all optional, all support non-interactive use):
#   OVPN_PORT, OVPN_PROTO (udp/tcp), OVPN_SUBNET ("network mask")
#   OVPN_SHARED_CA_DIR - path to a PKI directory to *import* instead of
#     generating a fresh CA (this is what multi-node/multi-remote setups
#     use - see ovpn_export_ca/ovpn_import_ca below)
# ---------------------------------------------------------------------
ovpn_install_core() {
    _ovpn_log "Installing openvpn + easy-rsa..."
    if ! command -v openvpn &>/dev/null; then
        if ! _ovpn_install_package; then return 1; fi
    fi
    if ! command -v openvpn &>/dev/null; then
        _ovpn_err "openvpn still not found after install - check ${LOG_FILE:-/tmp/simorgh_install.log}"
        return 1
    fi
    _ovpn_ok "openvpn + easy-rsa installed."

    local easyrsa_bin; easyrsa_bin="$(_ovpn_find_easyrsa)"
    if [ -z "$easyrsa_bin" ]; then
        _ovpn_err "easyrsa binary not found after install."
        return 1
    fi

    mkdir -p "$OVPN_DIR"

    if [ -n "${OVPN_SHARED_CA_DIR:-}" ] && [ -d "${OVPN_SHARED_CA_DIR:-}" ] && [ ! -d "$OVPN_PKI_DIR" ]; then
        _ovpn_log "Importing shared PKI from $OVPN_SHARED_CA_DIR (multi-node mode - one client cert will be valid on every node sharing this CA)..."
        mkdir -p "$OVPN_PKI_DIR"
        cp -r "$OVPN_SHARED_CA_DIR/." "$OVPN_PKI_DIR/"
        # The export only ships ca.crt/ca.key plus bookkeeping - easyrsa
        # still expects these directories to exist even when empty.
        mkdir -p "$OVPN_PKI_DIR/pki/reqs" "$OVPN_PKI_DIR/pki/issued" \
                 "$OVPN_PKI_DIR/pki/certs_by_serial" "$OVPN_PKI_DIR/pki/private"
        [ -f "$OVPN_PKI_DIR/pki/index.txt.attr" ] || echo "unique_subject = no" > "$OVPN_PKI_DIR/pki/index.txt.attr"
        _ovpn_ok "Shared CA imported."
    elif [ ! -d "$OVPN_PKI_DIR" ]; then
        _ovpn_log "Initializing a fresh (single-node) PKI..."
        mkdir -p "$OVPN_PKI_DIR"
        cp -r /usr/share/easy-rsa/* "$OVPN_PKI_DIR/" 2>/dev/null || true
        (
            cd "$OVPN_PKI_DIR" || exit 1
            export EASYRSA_BATCH=1
            "$easyrsa_bin" init-pki >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            "$easyrsa_bin" build-ca nopass >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
        )
        if [ ! -f "$OVPN_PKI_DIR/pki/ca.crt" ]; then
            _ovpn_err "CA generation failed - check ${LOG_FILE:-/tmp/simorgh_install.log}"
            return 1
        fi
        _ovpn_ok "Fresh CA generated."
    else
        _ovpn_log "PKI already present at $OVPN_PKI_DIR, keeping it."
    fi

    # Whether the CA above was just imported, just generated, or already
    # existed, THIS node still needs its own server cert/DH/TLS-auth key -
    # those are never shared across nodes, only the CA is.
    if [ ! -f "$OVPN_PKI_DIR/pki/issued/server.crt" ]; then
        _ovpn_log "Issuing this node's own server certificate..."
        (
            cd "$OVPN_PKI_DIR" || exit 1
            export EASYRSA_BATCH=1
            "$easyrsa_bin" gen-req server nopass >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
            "$easyrsa_bin" sign-req server server >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
        )
        if [ ! -f "$OVPN_PKI_DIR/pki/issued/server.crt" ]; then
            _ovpn_err "Server certificate generation failed - check ${LOG_FILE:-/tmp/simorgh_install.log}"
            return 1
        fi
        _ovpn_ok "Server certificate issued."
    fi
    if [ ! -f "$OVPN_PKI_DIR/pki/dh.pem" ]; then
        _ovpn_log "Generating Diffie-Hellman parameters (this can take a minute)..."
        ( cd "$OVPN_PKI_DIR" && "$easyrsa_bin" gen-dh >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1 )
        if [ ! -f "$OVPN_PKI_DIR/pki/dh.pem" ]; then
            _ovpn_err "DH parameter generation failed - check ${LOG_FILE:-/tmp/simorgh_install.log}"
            return 1
        fi
    fi
    if [ ! -f "$OVPN_PKI_DIR/pki/ta.key" ]; then
        openvpn --genkey secret "$OVPN_PKI_DIR/pki/ta.key" >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
    fi

    local port proto subnet ext_iface
    port="${OVPN_PORT:-}"
    if [ -z "$port" ] && [ -t 0 ]; then
        read -r -p "  OpenVPN port [$OVPN_PORT_DEFAULT]: " port
    fi
    port="${port:-$OVPN_PORT_DEFAULT}"

    proto="${OVPN_PROTO:-udp}"
    subnet="${OVPN_SUBNET:-$OVPN_SUBNET_DEFAULT}"

    ext_iface="${OVPN_EXT_IFACE:-}"
    if [ -z "$ext_iface" ]; then
        ext_iface="$(ip route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
    fi

    if [ ! -f "$OVPN_CONF" ]; then
        cat > "$OVPN_CONF" <<EOF
port $port
proto $proto
dev tun
ca $OVPN_PKI_DIR/pki/ca.crt
cert $OVPN_PKI_DIR/pki/issued/server.crt
key $OVPN_PKI_DIR/pki/private/server.key
dh $OVPN_PKI_DIR/pki/dh.pem
tls-auth $OVPN_PKI_DIR/pki/ta.key 0
server $subnet
push "redirect-gateway def1 bypass-dhcp"
push "dhcp-option DNS 1.1.1.1"
keepalive 10 60
cipher AES-256-GCM
persist-key
persist-tun
user nobody
group nogroup
status /var/log/openvpn-status.log
verb 3
EOF
        _ovpn_ok "server.conf created (port $port/$proto, subnet: $subnet)."
    else
        _ovpn_log "server.conf already exists, leaving it as-is."
    fi

    _ovpn_log "Enabling IP forwarding and NAT..."
    sysctl -w net.ipv4.ip_forward=1 >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
    if ! grep -q '^net.ipv4.ip_forward' /etc/sysctl.conf 2>/dev/null; then
        echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
    fi
    if [ -n "$ext_iface" ]; then
        iptables -t nat -C POSTROUTING -s "${subnet%% *}/24" -o "$ext_iface" -j MASQUERADE 2>/dev/null || \
            iptables -t nat -A POSTROUTING -s "${subnet%% *}/24" -o "$ext_iface" -j MASQUERADE
    fi

    _ovpn_log "Starting openvpn-server@server..."
    systemctl enable --now openvpn-server@server >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
    sleep 1
    if systemctl is-active --quiet openvpn-server@server; then
        _ovpn_ok "OpenVPN is up."
    else
        _ovpn_err "openvpn-server@server did not come up cleanly."
        _ovpn_log "Run: systemctl status openvpn-server@server"
        _ovpn_log "And: journalctl -u openvpn-server@server -n 50 --no-pager"
        return 1
    fi
}

# ---------------------------------------------------------------------
# Shared-CA helpers: what makes one .ovpn with 3 <connection> blocks
# possible. Export the PKI directory from whichever node bootstraps
# first, then pass it as OVPN_SHARED_CA_DIR to ovpn_install_core on
# every subsequent node.
# ---------------------------------------------------------------------
ovpn_export_ca_tarball() {
    local out="${1:-/tmp/simorgh-ovpn-ca.tar.gz}"
    local base; base="$(basename "$OVPN_PKI_DIR")"
    local stage; stage="$(mktemp -d)"

    # Stage only what a new node needs to trust/sign against this CA:
    # ca.crt + ca.key (+ the index/serial bookkeeping easyrsa itself wants).
    # Deliberately NOT copied: issued/, private/server.key, private/<name>.key,
    # reqs/ - those are per-node or per-customer and never shared.
    mkdir -p "$stage/$base/pki"
    cp "$OVPN_PKI_DIR/pki/ca.crt" "$stage/$base/pki/" 2>/dev/null
    mkdir -p "$stage/$base/pki/private"
    cp "$OVPN_PKI_DIR/pki/private/ca.key" "$stage/$base/pki/private/" 2>/dev/null
    for f in index.txt index.txt.attr serial openssl-easyrsa.cnf; do
        [ -f "$OVPN_PKI_DIR/pki/$f" ] && cp "$OVPN_PKI_DIR/pki/$f" "$stage/$base/pki/"
    done
    cp -r /usr/share/easy-rsa/x509-types "$stage/$base/" 2>/dev/null
    cp /usr/share/easy-rsa/vars.example "$stage/$base/" 2>/dev/null

    tar -czf "$out" -C "$stage" "$base" 2>/dev/null
    rm -rf "$stage"
    echo "$out"
}

# ---------------------------------------------------------------------
# Add one customer on THIS node: a client cert signed by this node's CA.
# ---------------------------------------------------------------------
ovpn_add_customer() {
    local name="$1"
    if [ -z "$name" ]; then
        read -r -p "  Customer name: " name
    fi
    if [ -z "$name" ]; then
        _ovpn_err "Name cannot be empty."
        return 1
    fi
    if [ -f "$OVPN_PKI_DIR/pki/issued/$name.crt" ]; then
        _ovpn_err "A client cert for \"$name\" already exists."
        return 1
    fi

    local easyrsa_bin; easyrsa_bin="$(_ovpn_find_easyrsa)"
    if [ -z "$easyrsa_bin" ] || [ ! -d "$OVPN_PKI_DIR" ]; then
        _ovpn_err "PKI not found - run ovpn_install_core first."
        return 1
    fi

    (
        cd "$OVPN_PKI_DIR" || exit 1
        export EASYRSA_BATCH=1
        "$easyrsa_bin" gen-req "$name" nopass >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
        "$easyrsa_bin" sign-req client "$name" >> "${LOG_FILE:-/tmp/simorgh_install.log}" 2>&1
    )
    if [ ! -f "$OVPN_PKI_DIR/pki/issued/$name.crt" ]; then
        _ovpn_err "Client cert generation failed - check ${LOG_FILE:-/tmp/simorgh_install.log}"
        return 1
    fi
    _ovpn_ok "Client cert issued for \"$name\"."

    local port proto
    port="$(grep -m1 '^port' "$OVPN_CONF" 2>/dev/null | awk '{print $2}')"
    proto="$(grep -m1 '^proto' "$OVPN_CONF" 2>/dev/null | awk '{print $2}')"

    echo
    echo "  ── OpenVPN client config (single node) ──"
    _ovpn_render_client_block "${OVPN_DIRECT_HOST:-<THIS_SERVER_PUBLIC_IP>}" "${port:-1194}" "${proto:-udp}" "$name"
}

# _ovpn_render_client_block prints one full standalone .ovpn (used for the
# single-node case; the multi-node combiner in nodepanel reuses the same
# ca/cert/key material but wraps multiple remotes in <connection> blocks
# instead of the top-level "remote" directive).
_ovpn_render_client_block() {
    local host="$1" port="$2" proto="$3" name="$4"
    cat <<EOF
client
dev tun
proto $proto
remote $host $port
resolv-retry infinite
nobind
persist-key
persist-tun
remote-cert-tls server
cipher AES-256-GCM
verb 3
<ca>
$(cat "$OVPN_PKI_DIR/pki/ca.crt")
</ca>
<cert>
$(sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p' "$OVPN_PKI_DIR/pki/issued/$name.crt")
</cert>
<key>
$(cat "$OVPN_PKI_DIR/pki/private/$name.key")
</key>
<tls-auth>
$(cat "$OVPN_PKI_DIR/pki/ta.key")
</tls-auth>
key-direction 1
EOF
}
