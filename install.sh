#!/usr/bin/env bash
# Simorgh Gaming Tunnel - management script
set -uo pipefail

# ---------------------------------------------------------------------------
# constants
# ---------------------------------------------------------------------------
IMG="simorgh-core:latest"
CONF_FILE="/etc/simorgh.conf"
SERVICE_FILE="/etc/systemd/system/simorgh.service"
INSTALL_DIR="/usr/local/bin"
SCRIPT_NAME="simorgh"
LOG_FILE="/tmp/simorgh_install.log"
CONTAINER_NAME="simorgh_tunnel"

DATA_DIR="/usr/local/share/simorgh"
CORE_DIR="$DATA_DIR/core"
PROTOCOLS_DIR="$DATA_DIR/protocols"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)"

# The multi-protocol panel (WireGuard, OpenVPN, L2TP, Xray, IPsec/IKEv2, PPTP,
# SSTP, OpenConnect, MTProto, AmneziaWG, GRE, SSH, RADIUS) lives in panel/ and is
# a separate program from the tunnel core. It is BUILT FROM SOURCE here rather
# than downloaded: panel/deploy.sh fetches a release binary from the upstream
# vpn-ui repository, which is a different program from this fork and would not
# carry anything added here.
PANEL_SRC_DIR="$DATA_DIR/panel"
PANEL_BIN="/usr/local/bin/simorgh-panel"
PANEL_SERVICE="vpn-ui"
GO_MIN="1.26.2"
GO_VERSION="1.26.5"

# Where to fetch the project from when this script is run stand-alone (the
# quick-install one-liner) and there's no local core/ checkout next to it.
# Override with: SIMORGH_REPO=https://github.com/you/your-fork.git ./install.sh
REPO_URL="${SIMORGH_REPO:-https://github.com/devprogrmer/simorgh.git}"
RAW_SELF_URL="${SIMORGH_RAW_SELF:-https://raw.githubusercontent.com/devprogrmer/simorgh/main/install.sh}"

# ---------------------------------------------------------------------------
# colors
# ---------------------------------------------------------------------------
G="\033[0;32m"; R="\033[0;31m"; Y="\033[0;33m"
W="\033[1;37m"; DIM="\033[2m"; BOLD="\033[1m"; NC="\033[0m"

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        echo -e "${R}Please run as root (sudo ./install.sh)${NC}"
        exit 1
    fi
}

banner() {
    clear
    echo -e "${BOLD}${W}  Simorgh${NC}  ${DIM}Gaming Tunnel · self-built core, zero third-party images${NC}"
    echo -e "${DIM}  ─────────────────────────────────────────────────────${NC}"
}

pause() { read -r -p "  Press Enter to continue..." _; }

# ---------------------------------------------------------------------------
# dependency + core-image install
# ---------------------------------------------------------------------------
install_deps() {
    echo -e "  ${Y}Checking dependencies...${NC}"
    : > "$LOG_FILE"

    if ! command -v docker &>/dev/null; then
        echo -e "  ${Y}Installing Docker...${NC}"
        curl -fsSL https://get.docker.com | sh >> "$LOG_FILE" 2>&1
        systemctl enable --now docker >> "$LOG_FILE" 2>&1
    fi

    if ! command -v ip &>/dev/null || ! command -v iptables &>/dev/null || \
       ! command -v curl &>/dev/null || ! command -v rsync &>/dev/null || \
       ! command -v git &>/dev/null || ! command -v python3 &>/dev/null; then
        echo -e "  ${Y}Installing network tools...${NC}"
        if [ -f /etc/debian_version ]; then
            apt-get update -q >> "$LOG_FILE" 2>&1
            apt-get install -y -q iproute2 iptables curl rsync git python3 >> "$LOG_FILE" 2>&1
        elif [ -f /etc/redhat-release ]; then
            yum install -y -q iproute iptables curl rsync git python3 >> "$LOG_FILE" 2>&1
        fi
    fi

    _install_self

    _sync_core_source
    _sync_protocols_source
    echo -e "  ${G}[OK] Dependencies ready.${NC}"
}

# Installs a working copy of this script to $INSTALL_DIR. Works whether $0
# is a real file (git clone, or the download-then-run quick install) or,
# defensively, a raw `curl | bash` pipe where $0 has no usable path.
_install_self() {
    local self_path
    self_path="$(realpath "$0" 2>/dev/null || true)"
    if [ -n "$self_path" ] && [ -f "$self_path" ] && [ "$(basename "$self_path")" != "bash" ] && [ "$(basename "$self_path")" != "sh" ]; then
        if [ "$self_path" != "$INSTALL_DIR/$SCRIPT_NAME" ]; then
            cp "$self_path" "$INSTALL_DIR/$SCRIPT_NAME"
            chmod +x "$INSTALL_DIR/$SCRIPT_NAME"
        fi
        return
    fi
    # Piped execution with no real $0 to copy - fetch a stable copy instead.
    curl -fsSL "$RAW_SELF_URL" -o "$INSTALL_DIR/$SCRIPT_NAME" 2>>"$LOG_FILE" && \
        chmod +x "$INSTALL_DIR/$SCRIPT_NAME"
}

# Copies core/ (Go source + Dockerfile) from the checkout this script runs
# from into a permanent location, so `docker build` keeps working even
# after $0 has been copied to $INSTALL_DIR and the git checkout is gone.
# If there's no local checkout at all (the quick-install one-liner just
# downloaded install.sh by itself), clone the repo to fetch core/.
# _fetch_repo puts a full checkout in $FETCHED_SRC, downloading it once per run.
#
# A tarball, not a git clone. Measured on this repo: 5.5 MB in 8.7s versus a
# shallow clone taking 10.3s and leaving another 5.9 MB of .git behind. But the
# size is the least of it —
#
#   - It is one plain HTTPS GET. Servers where the git protocol is slow, proxied
#     or blocked outright can still fetch it, and that is common on exactly the
#     hosts this project is for.
#   - It needs no git installed at all.
#   - Someone who ran the curl quick-install has already proved HTTPS to GitHub
#     works. Making the next step depend on a different protocol is asking for a
#     failure they have no way to interpret.
#
# git clone stays as a fallback, since a private fork set through SIMORGH_REPO
# may have no codeload equivalent.
#
# Cached in a run-scoped variable so installing the core AND the panel downloads
# once rather than twice, which is what the two separate clones used to do.
FETCHED_SRC=""
_fetch_repo() {
    [ -n "$FETCHED_SRC" ] && [ -d "$FETCHED_SRC" ] && return 0

    local tmp; tmp="$(mktemp -d)"

    # Derive the codeload URL from the configured repo. Only works for GitHub;
    # anything else drops through to git.
    local slug="" tarball=""
    case "$REPO_URL" in
        https://github.com/*)
            slug="${REPO_URL#https://github.com/}"; slug="${slug%.git}"
            tarball="https://codeload.github.com/$slug/tar.gz/refs/heads/${SIMORGH_BRANCH:-main}"
            ;;
    esac

    if [ -n "$tarball" ]; then
        local branch="${SIMORGH_BRANCH:-main}"
        local direct="https://codeload.github.com/$slug/tar.gz/refs/heads/$branch"
        local archive="https://github.com/$slug/archive/refs/heads/$branch.tar.gz"

        # GitHub first, then public mirrors.
        #
        # The mirrors are here because GitHub is intermittently unreachable from
        # the networks this project is FOR -- measured on one such link, the same
        # codeload URL served in 4s and then timed out entirely 20 minutes later.
        # Falling back means an install that would simply have failed now works.
        #
        # They are a real trust decision, not a free win: a mirror is a third
        # party serving code this script then builds and runs as root. So GitHub
        # is always tried first, a mirror is only ever a fallback, and using one
        # is announced rather than silent. SIMORGH_NO_MIRRORS=1 turns them off
        # for anyone who would rather fail than fetch from a third party.
        local sources="$direct"
        if [ "${SIMORGH_NO_MIRRORS:-0}" != "1" ]; then
            sources="$sources https://ghproxy.net/$archive https://ghfast.top/$archive https://gh.llkk.cc/$archive"
        fi

        local url first=1
        for url in $sources; do
            if [ $first -eq 1 ]; then
                echo -e "  ${Y}Downloading source (~6 MB)...${NC}"
                first=0
            else
                echo -e "  ${Y}Trying mirror: ${url%%/https*}${NC}"
            fi
            if curl -fL --progress-bar --max-time 600 -o "$tmp/src.tgz" "$url" 2>&1; then
                # --strip-components=1 drops the "simorgh-main/" wrapper.
                if tar -xzf "$tmp/src.tgz" -C "$tmp" --strip-components=1 2>>"$LOG_FILE" && [ -d "$tmp/core" ]; then
                    rm -f "$tmp/src.tgz"
                    case "$url" in
                        "$direct") : ;;
                        *) echo -e "  ${Y}Note: fetched via a mirror, not github.com directly.${NC}" ;;
                    esac
                    FETCHED_SRC="$tmp"
                    return 0
                fi
                echo -e "  ${DIM}Downloaded but could not extract; trying the next source.${NC}"
            fi
            rm -f "$tmp/src.tgz"
        done
        echo -e "  ${DIM}No tarball source worked; falling back to git.${NC}"
        rm -rf "$tmp"; tmp="$(mktemp -d)"
    fi

    if ! command -v git >/dev/null 2>&1; then
        echo -e "  ${R}[ERROR] Could not download the source, and git is not installed to fall back on.${NC}"
        rm -rf "$tmp"; return 1
    fi
    echo -e "  ${Y}Cloning $REPO_URL ...${NC}"
    if ! timeout 1200 git clone --depth=1 --progress "$REPO_URL" "$tmp" 2>&1 | tee -a "$LOG_FILE"; then
        echo -e "  ${R}[ERROR] Could not fetch $REPO_URL${NC}"
        echo -e "  ${DIM}If GitHub is unreachable from this server, copy a checkout next to"
        echo -e "  this script, or point SIMORGH_REPO at a mirror you can reach.${NC}"
        rm -rf "$tmp"; return 1
    fi
    FETCHED_SRC="$tmp"
    return 0
}

_sync_core_source() {
    mkdir -p "$DATA_DIR"

    if [ -d "$SCRIPT_DIR/core" ]; then
        rsync -a --delete "$SCRIPT_DIR/core/" "$CORE_DIR/" 2>>"$LOG_FILE" || \
            cp -r "$SCRIPT_DIR/core" "$DATA_DIR/"
        return
    fi

    if [ -d "$CORE_DIR" ]; then
        return # already have a copy from a previous install/run
    fi

    echo -e "  ${Y}No local core/ next to this script.${NC}"
    if ! _fetch_repo; then
        exit 1
    fi
    local tmp="$FETCHED_SRC"
    if [ ! -d "$tmp/core" ]; then
        echo -e "  ${R}[ERROR] Fetched the source but it has no core/ directory.${NC}"
        exit 1
    fi
    rsync -a "$tmp/core/" "$CORE_DIR/" 2>>"$LOG_FILE" || cp -r "$tmp/core" "$DATA_DIR/"
    if [ -d "$tmp/protocols" ]; then
        rsync -a "$tmp/protocols/" "$PROTOCOLS_DIR/" 2>>"$LOG_FILE" || cp -r "$tmp/protocols" "$DATA_DIR/"
    fi
}

# Same idea as _sync_core_source but for protocols/ (WireGuard/OpenVPN/L2TP
# installer modules). Not fatal if missing - older checkouts or a minimal
# clone might not have it yet, and the "Install Core" flow still works
# without it; only the protocol-installer menu items need it.
_sync_protocols_source() {
    mkdir -p "$DATA_DIR"
    if [ -d "$SCRIPT_DIR/protocols" ]; then
        rsync -a --delete "$SCRIPT_DIR/protocols/" "$PROTOCOLS_DIR/" 2>>"$LOG_FILE" || \
            cp -r "$SCRIPT_DIR/protocols" "$DATA_DIR/"
    fi
}

# Same idea again, for panel/. Kept separate from core because the panel is a
# much bigger tree and only copied when someone actually asks for it.
_sync_panel_source() {
    mkdir -p "$DATA_DIR"

    if [ -d "$SCRIPT_DIR/panel" ]; then
        rsync -a --delete "$SCRIPT_DIR/panel/" "$PANEL_SRC_DIR/" 2>>"$LOG_FILE" || \
            cp -r "$SCRIPT_DIR/panel" "$DATA_DIR/"
        return 0
    fi
    if [ -d "$PANEL_SRC_DIR" ]; then
        return 0
    fi

    echo -e "  ${Y}No local panel/ next to this script.${NC}"
    if ! _fetch_repo; then
        return 1
    fi
    local tmp="$FETCHED_SRC"
    if [ ! -d "$tmp/panel" ]; then
        echo -e "  ${R}[ERROR] Fetched the source but it has no panel/ directory.${NC}"
        return 1
    fi
    rsync -a "$tmp/panel/" "$PANEL_SRC_DIR/" 2>>"$LOG_FILE" || cp -r "$tmp/panel" "$DATA_DIR/"
}

# ---------------------------------------------------------------------------
# panel: Go toolchain + build + install
# ---------------------------------------------------------------------------

# ver_ge compares dotted versions: "1.26.5" >= "1.26.2". Pure sort -V, so it
# handles the 10-vs-9 case that a string compare gets wrong.
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

# ensure_go makes a new-enough Go available and echoes its path.
#
# The distro package is checked first and usually rejected: panel/go.mod requires
# go 1.26.2, and every stable distro currently ships something older (Ubuntu
# 24.04 has 1.22), which fails the build with a message about the toolchain
# rather than about the panel. So the official tarball is fetched to a private
# directory instead of replacing whatever the system has.
ensure_go() {
    local sys_go; sys_go="$(command -v go 2>/dev/null || true)"
    if [ -n "$sys_go" ]; then
        local v; v="$("$sys_go" env GOVERSION 2>/dev/null | sed 's/^go//')"
        if [ -n "$v" ] && ver_ge "$v" "$GO_MIN"; then
            echo "$sys_go"; return 0
        fi
        echo -e "  ${DIM}System Go is $v; the panel needs $GO_MIN or newer.${NC}" >&2
    fi

    local priv="$DATA_DIR/go/bin/go"
    if [ -x "$priv" ] && ver_ge "$("$priv" env GOVERSION 2>/dev/null | sed 's/^go//')" "$GO_MIN"; then
        echo "$priv"; return 0
    fi

    echo -e "  ${Y}Fetching Go $GO_VERSION (needed to build the panel)...${NC}" >&2
    local arch; case "$(uname -m)" in
        x86_64)  arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        armv7l)  arch=armv6l ;;
        *) echo -e "  ${R}Unsupported architecture $(uname -m)${NC}" >&2; return 1 ;;
    esac
    local tgz="/tmp/go-$GO_VERSION.tar.gz"
    local file="go$GO_VERSION.linux-$arch.tar.gz"

    # go.dev first, then mirrors -- and the mirrors are not optional politeness.
    #
    # go.dev/dl redirects the actual tarball to dl.google.com, which answers 404
    # on a good number of the networks this project is for. Measured on one such
    # link: a clean 404 for a file that plainly exists. Without a fallback the
    # installer stops here and the operator is told "could not download Go" with
    # no idea that the file is fine and the path is not.
    #
    # Whatever a mirror serves is verified against go.dev's own published SHA256
    # below, so a mirror cannot substitute a different toolchain -- it can only
    # fail the check and be skipped.
    local sources="https://go.dev/dl/$file"
    if [ "${SIMORGH_NO_MIRRORS:-0}" != "1" ]; then
        sources="$sources https://mirrors.aliyun.com/golang/$file https://mirror.nju.edu.cn/golang/$file"
    fi

    # go.dev publishes checksums as JSON. Fetched separately and cheaply; if it
    # is unreachable the download still proceeds, just unverified, and says so.
    local want_sha=""
    # -A12, not fewer: sha256 sits four fields after filename and the split
    # leaves blank lines between them, so a narrow window silently finds
    # nothing and the download goes unverified without anyone noticing.
    want_sha="$(curl -fsSL --max-time 45 "https://go.dev/dl/?mode=json&include=all" 2>/dev/null \
        | tr ',{}' '\n\n\n' | grep -A12 "\"$file\"" | grep -o '"sha256": *"[a-f0-9]*"' \
        | head -1 | grep -o '[a-f0-9]\{64\}' || true)"

    local url got=0
    for url in $sources; do
        echo -e "  ${DIM}  ${url}${NC}" >&2
        curl -fL --progress-bar --max-time 900 -o "$tgz" "$url" 2>&1 >/dev/null || { rm -f "$tgz"; continue; }
        if [ -n "$want_sha" ]; then
            local have; have="$(sha256sum "$tgz" 2>/dev/null | cut -d' ' -f1)"
            if [ "$have" != "$want_sha" ]; then
                echo -e "  ${R}  checksum mismatch from this source - skipping it${NC}" >&2
                rm -f "$tgz"; continue
            fi
        fi
        got=1; break
    done
    if [ "$got" -ne 1 ]; then
        echo -e "  ${R}Could not download Go from any source.${NC}" >&2
        echo -e "  ${DIM}Install Go $GO_MIN+ yourself and re-run; the script will use it.${NC}" >&2
        return 1
    fi
    if [ -z "$want_sha" ]; then
        echo -e "  ${Y}  Note: go.dev's checksum list was unreachable, so the download was not verified.${NC}" >&2
    fi
    rm -rf "$DATA_DIR/go"
    mkdir -p "$DATA_DIR/go"
    tar -C "$DATA_DIR/go" --strip-components=1 -xzf "$tgz" 2>>"$LOG_FILE" || {
        echo -e "  ${R}Could not unpack Go.${NC}" >&2; rm -f "$tgz"; return 1; }
    rm -f "$tgz"
    [ -x "$priv" ] || { echo -e "  ${R}Go unpacked but $priv is missing.${NC}" >&2; return 1; }
    echo "$priv"
}

install_panel() {
    banner
    echo -e "  ${BOLD}Install the multi-protocol panel${NC}"
    echo
    echo -e "  ${DIM}WireGuard · OpenVPN · L2TP · Xray (VMess/VLESS/Trojan/Shadowsocks)${NC}"
    echo -e "  ${DIM}IPsec/IKEv2 · PPTP · SSTP · OpenConnect (Cisco) · MTProto${NC}"
    echo -e "  ${DIM}AmneziaWG · GRE · SSH · RADIUS — with users, quotas,${NC}"
    echo -e "  ${DIM}subscription links and reseller accounts.${NC}"
    echo
    echo -e "  ${DIM}This builds from source on this machine. First run takes a${NC}"
    echo -e "  ${DIM}few minutes and needs to reach the Go module proxy.${NC}"
    echo

    _sync_panel_source || { pause; return 1; }

    local go_bin; go_bin="$(ensure_go)" || { pause; return 1; }
    echo -e "  ${G}Using $("$go_bin" env GOVERSION)${NC}"

    # GOPROXY, and why it is not the default.
    #
    # proxy.golang.org answers module metadata itself but redirects the module
    # ZIPS to storage.googleapis.com, which is blocked on a good number of the
    # networks this project is for. The symptom is brutal: roughly 120 modules
    # download fine and two fail with 403, so the build dies minutes in, on a
    # dependency nobody has heard of, with nothing pointing at the real cause.
    #
    # "direct" makes the go tool fetch straight from each module's own origin
    # (github.com and friends) instead. Slower when the proxy IS reachable, and
    # worth it: a slow build that finishes beats a fast one that cannot.
    #
    # Overridable, because an operator with a working proxy or a corporate
    # mirror should not be forced onto the slow path:
    #   SIMORGH_GOPROXY="https://proxy.golang.org,direct" sudo ./install.sh
    local goproxy="${SIMORGH_GOPROXY:-direct}"

    echo -e "  ${Y}Building the panel (this is the slow part)...${NC}"
    echo -e "  ${DIM}Fetching ~120 Go modules with GOPROXY=$goproxy, then compiling."
    echo -e "  Several minutes is normal. Output is live below, so a stall is visible.${NC}"
    echo
    # Live output, not swallowed into the log. This build used to write only to
    # $LOG_FILE, which made a slow module download indistinguishable from a hang
    # -- the same failure the source fetch had, in the step that takes longest.
    if ! ( cd "$PANEL_SRC_DIR" && GOFLAGS=-mod=mod GOPROXY="$goproxy" "$go_bin" build -v -o "$PANEL_BIN" . ) 2>&1 | tee -a "$LOG_FILE"; then
        echo
        echo -e "  ${R}[ERROR] Panel build failed.${NC}"
        echo -e "  ${DIM}If it failed downloading modules, this server may not reach them."
        echo -e "  Try a proxy that works from here:"
        echo -e "    SIMORGH_GOPROXY=https://goproxy.io,direct sudo ./install.sh${NC}"
        pause; return 1
    fi
    chmod +x "$PANEL_BIN"
    echo -e "  ${G}Built $PANEL_BIN${NC}"

    echo -e "  ${Y}Installing the service...${NC}"
    if ! "$PANEL_BIN" --systemd; then
        echo -e "  ${R}[ERROR] Could not install the panel service.${NC}"
        pause; return 1
    fi

    # A panel reachable on a guessable port with admin/admin is a compromised
    # panel, so the default is to randomise the port, path and credentials rather
    # than to leave the operator to remember to.
    echo
    # --random is a TOP-LEVEL flag, not a `setting` one: `setting` has its own
    # flag set (-port, -username, -password, -webBasePath) and would reject it.
    if [ "$(ask 'Randomise the port, URL path and login? (recommended) [y/n]' 'y')" = "y" ]; then
        "$PANEL_BIN" --random || true
    fi

    echo
    echo -e "  ${G}${BOLD}Panel installed.${NC}"
    "$PANEL_BIN" info 2>/dev/null || true
    echo
    echo -e "  ${DIM}Write the URL and login down now — the password is not${NC}"
    echo -e "  ${DIM}recoverable from here, only resettable.${NC}"
    pause
}

manage_panel() {
    while true; do
        banner
        if [ ! -x "$PANEL_BIN" ]; then
            echo -e "  ${Y}The panel is not installed yet.${NC}"
            pause; return
        fi
        local state; state="$(systemctl is-active "$PANEL_SERVICE" 2>/dev/null || echo unknown)"
        case "$state" in
            active) echo -e "  Panel: ${G}running${NC}" ;;
            *)      echo -e "  Panel: ${R}$state${NC}" ;;
        esac
        echo
        echo "  1) Show URL and login"
        echo "  2) Start"
        echo "  3) Stop"
        echo "  4) Restart"
        echo "  5) Live logs"
        echo "  6) Change port / path / login"
        echo "  7) Rebuild from source (after updating this repo)"
        echo "  8) Uninstall the panel"
        echo "  0) Back"
        echo
        local c; c=$(ask "Choose" "0")
        case "$c" in
            1) "$PANEL_BIN" info; pause ;;
            2) systemctl start "$PANEL_SERVICE"; pause ;;
            3) systemctl stop "$PANEL_SERVICE"; pause ;;
            4) systemctl restart "$PANEL_SERVICE"; pause ;;
            5) echo -e "  ${DIM}Ctrl-C to stop following.${NC}"; journalctl -u "$PANEL_SERVICE" -f ;;
            6)
               # `setting` with no flags would call updateSetting with every
               # value empty, which changes nothing and looks like a failure.
               # Collect the values first and pass only what was filled in.
               local u p prt bp; local -a args=()
               u=$(ask "New username (blank = leave alone)" "")
               p=$(ask "New password (blank = leave alone)" "")
               prt=$(ask "New port (blank = leave alone)" "")
               bp=$(ask "New URL path (blank = leave alone)" "")
               [ -n "$u" ]   && args+=(-username "$u")
               [ -n "$p" ]   && args+=(-password "$p")
               [ -n "$prt" ] && args+=(-port "$prt")
               [ -n "$bp" ]  && args+=(-webBasePath "$bp")
               if [ ${#args[@]} -eq 0 ]; then
                   echo -e "  ${DIM}Nothing entered; leaving settings as they are.${NC}"
               else
                   "$PANEL_BIN" setting "${args[@]}" && systemctl restart "$PANEL_SERVICE"
               fi
               pause ;;
            7) install_panel ;;
            8)
               # Delegates to the binary's own uninstall, which also removes the
               # child daemons, nftables rules, policy routing and /etc configs
               # it created. Deleting the binary alone would leave all of that
               # running with nothing left to manage it.
               "$PANEL_BIN" --uninstall
               rm -f "$PANEL_BIN"
               echo -e "  ${G}Panel removed.${NC}"; pause; return ;;
            0) return ;;
        esac
    done
}

install_core() {
    banner
    install_deps
    echo
    echo -e "  ${Y}Building Simorgh core image locally (${IMG})...${NC}"
    echo -e "  ${DIM}Source: $CORE_DIR - nothing is pulled from any registry.${NC}"
    if ! docker build -t "$IMG" "$CORE_DIR" >> "$LOG_FILE" 2>&1; then
        echo -e "  ${R}[ERROR] Build failed. Log: $LOG_FILE${NC}"
        pause; return 1
    fi
    echo -e "  ${G}[OK] Core image built.${NC}"
    pause
}

# ---------------------------------------------------------------------------
# config helpers
# ---------------------------------------------------------------------------
conf_get() { grep -E "^$1=" "$CONF_FILE" 2>/dev/null | tail -1 | cut -d= -f2-; }

write_conf() {
    cat > "$CONF_FILE" <<EOF
ROLE=$ROLE
IFACE=$IFACE
MODE=$MODE
TRANSPORT=$TRANSPORT
REMOTE_IP=$REMOTE_IP
UDP_PORT=$UDP_PORT
PASSWORD=$PASSWORD
MTU=$MTU
KEEPALIVE=$KEEPALIVE
LINK_QUALITY=$LINK_QUALITY
BAN_QUALITY=$BAN_QUALITY
DSCP_MARK=$DSCP_MARK
BANDWIDTH_LIMIT=$BANDWIDTH_LIMIT
FEC_ENABLE=$FEC_ENABLE
FEC_GROUP=$FEC_GROUP
OPERATING_MODE=$OPERATING_MODE
LOCAL_TUN_IP=$LOCAL_TUN_IP
TUN_MASK=$TUN_MASK
FORWARD_BIND=$FORWARD_BIND
LOCAL_PORT=$LOCAL_PORT
TARGET_HOST=$TARGET_HOST
TARGET_PORT=$TARGET_PORT
FORWARD_PROTO=$FORWARD_PROTO
PING_INTERVAL=$PING_INTERVAL
HEALTH_CHECK_INTERVAL=$HEALTH_CHECK_INTERVAL
SWITCH_MARGIN_MS=$SWITCH_MARGIN_MS
SWITCH_CONFIRM_ROUNDS=$SWITCH_CONFIRM_ROUNDS
EOF
    chmod 600 "$CONF_FILE"
}

load_conf() {
    [ -f "$CONF_FILE" ] || return 1
    # shellcheck disable=SC1090
    set -a; source "$CONF_FILE"; set +a
    return 0
}

# ---------------------------------------------------------------------------
# create tunnel
# ---------------------------------------------------------------------------
ask() { local prompt="$1" def="${2:-}" ans; read -r -p "  $prompt${def:+ [$def]}: " ans; echo "${ans:-$def}"; }

create_tunnel() {
    banner
    if ! docker image inspect "$IMG" &>/dev/null; then
        echo -e "  ${R}[ERROR] Core image not found. Run 'Install Core' first.${NC}"
        pause; return
    fi

    echo -e "  ${W}Role:${NC}"
    echo "    1) IRAN server   (client - connects out to your foreign server)"
    echo "    2) FOREIGN server (server - accepts the connection)"
    local rc; rc=$(ask "Choose 1 or 2" "1")
    if [ "$rc" = "2" ]; then ROLE="FOREIGN"; else ROLE="IRAN"; fi

    IFACE=$(ask "Tunnel interface name" "simorgh0")

    echo
    echo -e "  ${W}Mode:${NC}"
    echo "    1) Full VPN tunnel (own IP space, TUN device)"
    echo "    2) Carrier mode - relay WireGuard/OpenVPN's UDP traffic through"
    echo "       Simorgh for lower ping. Simorgh does NOT replace that VPN's"
    echo "       encryption in this mode, it only carries its packets."
    local mc; mc=$(ask "Choose 1 or 2" "1")
    if [ "$mc" = "2" ]; then MODE="forward"; else MODE="tun"; fi

    echo -e "  ${W}Transport:${NC}"
    echo "    1) icmp (default - lowest overhead, least likely to be filtered)"
    echo "    2) udp  (fallback if ICMP is blocked on this path)"
    if [ "$ROLE" = "IRAN" ]; then
        echo "    3) auto (client tries icmp, falls back to udp automatically -"
        echo "       needs the server side reachable on both, see docs/DEPLOYMENT.md)"
    fi
    local tc; tc=$(ask "Choose" "1")
    if [ "$tc" = "2" ]; then
        TRANSPORT="udp"
    elif [ "$tc" = "3" ] && [ "$ROLE" = "IRAN" ]; then
        TRANSPORT="auto"
    else
        TRANSPORT="icmp"
    fi
    UDP_PORT=$(ask "UDP port (used if transport=udp or auto)" "51900")

    REMOTE_IP=""
    if [ "$ROLE" = "IRAN" ]; then
        echo -e "  ${DIM}One server, or a comma-separated list for automatic failover${NC}"
        echo -e "  ${DIM}(e.g. 1.2.3.4,5.6.7.8 or 2001:db8::1) - IPv4 and IPv6 both work,${NC}"
        echo -e "  ${DIM}including mixed - Simorgh picks whichever measures best.${NC}"
        REMOTE_IP=$(ask "Foreign server IP(s)")
    fi

    PASSWORD=$(ask "Tunnel password (same on both ends)")
    while [ -z "$PASSWORD" ]; do
        echo -e "  ${R}Password cannot be empty.${NC}"
        PASSWORD=$(ask "Tunnel password (same on both ends)")
    done

    FORWARD_BIND="127.0.0.1"; LOCAL_PORT="51000"
    TARGET_HOST="127.0.0.1"; TARGET_PORT="51820"
    FORWARD_PROTO="udp"
    if [ "$MODE" = "forward" ]; then
        echo
        echo -e "  ${W}What is the real VPN's own transport?${NC}"
        echo "    1) UDP  (WireGuard, OpenVPN-UDP, L2TP/IPSec, most modern protocols)"
        echo "    2) TCP  (OpenVPN-TCP, Cisco AnyConnect/IPSec-over-TCP, Xray/VLESS-TCP, Trojan)"
        local fp; fp=$(ask "Choose 1 or 2" "1")
        if [ "$fp" = "2" ]; then FORWARD_PROTO="tcp"; else FORWARD_PROTO="udp"; fi
        if [ "$ROLE" = "IRAN" ]; then
            echo -e "  ${DIM}Point your VPN client's Endpoint/remote at this bind:port.${NC}"
            FORWARD_BIND=$(ask "Local bind address" "127.0.0.1")
            LOCAL_PORT=$(ask "Local port" "51000")
        else
            echo -e "  ${DIM}Where your real VPN server is actually listening.${NC}"
            TARGET_HOST=$(ask "Target host" "127.0.0.1")
            TARGET_PORT=$(ask "Target port" "51820")
        fi
    fi

    echo
    echo -e "  ${DIM}Advanced (press Enter to accept defaults)${NC}"
    MTU=$(ask "MTU" "1400")
    KEEPALIVE=$(ask "Keepalive interval (seconds)" "5")
    PING_INTERVAL=$(ask "Live RTT ping interval (seconds)" "1")
    LINK_QUALITY=$(ask "Link-quality warning threshold %, 0=off" "0")
    BAN_QUALITY=$(ask "Link-quality disconnect threshold %, 0=off" "0")
    DSCP_MARK=$(ask "DSCP mark 0-63, -1=off (client only, icmp only)" "-1")
    BANDWIDTH_LIMIT=$(ask "Bandwidth limit in Mbps, empty=unlimited (server only)" "")
    local fe; fe=$(ask "Enable lightweight FEC (packet-loss recovery, adapts automatically)? y/n" "n")
    if [[ "$fe" =~ ^[Yy] ]]; then FEC_ENABLE="true"; else FEC_ENABLE="false"; fi
    FEC_GROUP=$(ask "FEC base group size (data packets per parity packet)" "8")

    HEALTH_CHECK_INTERVAL="5"; SWITCH_MARGIN_MS="15"; SWITCH_CONFIRM_ROUNDS="3"
    if [ "$ROLE" = "IRAN" ] && [[ "$REMOTE_IP" == *,* ]]; then
        echo -e "  ${DIM}Multi-server failover tuning${NC}"
        HEALTH_CHECK_INTERVAL=$(ask "Health-check interval (seconds)" "5")
        SWITCH_MARGIN_MS=$(ask "Minimum score improvement to switch (ms-equivalent)" "15")
        SWITCH_CONFIRM_ROUNDS=$(ask "Consecutive good checks required before switching" "3")
    fi

    OPERATING_MODE=""; LOCAL_TUN_IP=""; TUN_MASK="30"
    if [ "$MODE" = "tun" ]; then
        OPERATING_MODE=$(ask "OPERATING_MODE (empty = manual IP assignment below)" "")
        if [ -z "$OPERATING_MODE" ]; then
            echo -e "  ${DIM}IPv4 (10.99.0.x) or IPv6 (e.g. fd00::1) both work for the tunnel's${NC}"
            echo -e "  ${DIM}own inner address - this is independent of the outer transport.${NC}"
            if [ "$ROLE" = "IRAN" ]; then
                LOCAL_TUN_IP=$(ask "This side's tunnel IP" "10.99.0.2")
            else
                LOCAL_TUN_IP=$(ask "This side's tunnel IP" "10.99.0.1")
            fi
            TUN_MASK=$(ask "Tunnel subnet mask (CIDR bits)" "30")
        fi
    fi

    write_conf
    _start_tunnel
    _create_service
    echo -e "  ${G}[OK] Tunnel created and started.${NC}"
    if [ "$MODE" = "forward" ] && [ "$ROLE" = "IRAN" ]; then
        echo -e "  ${DIM}Point your VPN client's Endpoint at ${FORWARD_BIND}:${LOCAL_PORT}${NC}"
    fi
    pause
}

_docker_run_args() {
    local -a a=(
        docker run -d --name "$CONTAINER_NAME"
        --cap-add=NET_ADMIN --cap-add=NET_RAW
        --device /dev/net/tun:/dev/net/tun
        --net=host --restart unless-stopped
        -e "INTERFACE=$IFACE"
        -e "MODE=$MODE"
        -e "TRANSPORT=$TRANSPORT"
        -e "PASSWORD=$PASSWORD"
        -e "UDP_PORT=$UDP_PORT"
        -e "MTU=$MTU"
        -e "KEEPALIVE=$KEEPALIVE"
        -e "LINK_QUALITY=$LINK_QUALITY"
        -e "BAN_QUALITY=$BAN_QUALITY"
        -e "DSCP_MARK=$DSCP_MARK"
        -e "FEC_ENABLE=$FEC_ENABLE"
        -e "FORWARD_BIND=$FORWARD_BIND"
        -e "LOCAL_PORT=$LOCAL_PORT"
        -e "TARGET_HOST=$TARGET_HOST"
        -e "TARGET_PORT=$TARGET_PORT"
        -e "FORWARD_PROTO=$FORWARD_PROTO"
        -e "OPERATING_MODE=$OPERATING_MODE"
        -e "PING_INTERVAL=$PING_INTERVAL"
        -e "HEALTH_CHECK_INTERVAL=$HEALTH_CHECK_INTERVAL"
        -e "SWITCH_MARGIN_MS=$SWITCH_MARGIN_MS"
        -e "SWITCH_CONFIRM_ROUNDS=$SWITCH_CONFIRM_ROUNDS"
        -e "FEC_GROUP=$FEC_GROUP"
        -e "OPERATING_MODE=$OPERATING_MODE"
    )
    [ -n "${REMOTE_IP:-}" ] && a+=(-e "REMOTE_IP=$REMOTE_IP")
    [ -n "${BANDWIDTH_LIMIT:-}" ] && a+=(-e "BANDWIDTH_LIMIT=$BANDWIDTH_LIMIT")
    a+=("$IMG")
    printf '%s\n' "${a[@]}"
}

_start_tunnel() {
    docker rm -f "$CONTAINER_NAME" &>/dev/null || true
    local -a args
    mapfile -t args < <(_docker_run_args)
    "${args[@]}" >> "$LOG_FILE" 2>&1

    sleep 1
    if [ -n "$LOCAL_TUN_IP" ] && [ -z "$OPERATING_MODE" ]; then
        ip addr add "$LOCAL_TUN_IP/$TUN_MASK" dev "$IFACE" 2>/dev/null || true
        ip link set "$IFACE" up 2>/dev/null || true
    fi
}

_create_service() {
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Simorgh Gaming Tunnel
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$INSTALL_DIR/$SCRIPT_NAME --start
ExecStop=/usr/bin/docker stop $CONTAINER_NAME
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable simorgh.service >> "$LOG_FILE" 2>&1
}

start_from_conf() {
    load_conf || { echo -e "${R}No config found. Create a tunnel first.${NC}"; exit 1; }
    _start_tunnel
}

# ---------------------------------------------------------------------------
# manage / dashboard / mtu optimizer
# ---------------------------------------------------------------------------
manage_tunnel() {
    load_conf || { banner; echo -e "  ${R}No tunnel configured yet.${NC}"; pause; return; }
    while true; do
        banner
        local status; status=$(docker inspect -f '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "not created")
        echo -e "  Role: ${W}$ROLE${NC}   Transport: ${W}$TRANSPORT${NC}   Status: ${W}$status${NC}"
        echo
        echo "  1) Restart tunnel"
        echo "  2) Stop tunnel"
        echo "  3) Start tunnel"
        echo "  4) View live logs (Ctrl+C to exit)"
        echo "  0) Back"
        local c; c=$(ask "Choose" "0")
        case "$c" in
            1) docker restart "$CONTAINER_NAME" >/dev/null 2>&1; echo -e "  ${G}restarted${NC}"; sleep 1 ;;
            2) docker stop "$CONTAINER_NAME" >/dev/null 2>&1; echo -e "  ${G}stopped${NC}"; sleep 1 ;;
            3) _start_tunnel; echo -e "  ${G}started${NC}"; sleep 1 ;;
            4) docker logs -f --tail 100 "$CONTAINER_NAME" ;;
            0) return ;;
        esac
    done
}

dashboard() {
    load_conf || { banner; echo -e "  ${R}No tunnel configured yet.${NC}"; pause; return; }
    banner
    echo -e "  ${W}Status${NC}"
    docker ps --filter "name=$CONTAINER_NAME" --format "  container: {{.Status}}"
    echo
    echo -e "  ${W}Resource usage${NC}"
    docker stats --no-stream --format "  CPU: {{.CPUPerc}}   MEM: {{.MemUsage}}   NET RX/TX: {{.NetIO}}" "$CONTAINER_NAME" 2>/dev/null
    echo
    if [ -n "${LOCAL_TUN_IP:-}" ] && [ "$ROLE" = "IRAN" ] && [ -n "${REMOTE_IP:-}" ]; then
        local peer_tun_ip
        peer_tun_ip=$(ask "Peer tunnel IP to ping (leave empty to skip)" "")
        if [ -n "$peer_tun_ip" ]; then
            echo -e "  ${W}Live ping through the tunnel${NC}"
            ping -c 5 -i 0.3 "$peer_tun_ip" 2>&1 | tail -6
        fi
    fi
    echo
    ip -s link show "$IFACE" 2>/dev/null | sed 's/^/  /'
    echo
    pause
}

mtu_optimizer() {
    load_conf || { banner; echo -e "  ${R}No tunnel configured yet.${NC}"; pause; return; }
    banner
    local target; target=$(ask "IP to test path MTU against (peer's real IP)" "${REMOTE_IP:-}")
    [ -z "$target" ] && { pause; return; }

    echo -e "  ${Y}Binary-searching for the largest non-fragmenting ICMP size...${NC}"
    local lo=576 hi=1500 best=576
    while [ "$lo" -le "$hi" ]; do
        local mid=$(( (lo + hi) / 2 ))
        if ping -c1 -W1 -M "do" -s "$((mid - 28))" "$target" &>/dev/null; then
            best=$mid; lo=$((mid + 1))
        else
            hi=$((mid - 1))
        fi
    done
    echo -e "  ${G}Largest working ICMP payload: ~$best bytes (try MTU=$((best - 20)) for the tunnel)${NC}"
    echo -e "  ${DIM}Update MTU in $CONF_FILE and restart the tunnel to apply.${NC}"
    pause
}

# ---------------------------------------------------------------------------
# uninstall
# ---------------------------------------------------------------------------
uninstall_all() {
    banner
    local c; c=$(ask "This removes the tunnel, service, image, and config. Type 'yes' to confirm" "no")
    [ "$c" != "yes" ] && return

    systemctl disable --now simorgh.service &>/dev/null || true
    docker rm -f "$CONTAINER_NAME" &>/dev/null || true
    load_conf &>/dev/null && ip link delete "$IFACE" 2>/dev/null || true
    rm -f "$CONF_FILE" "$SERVICE_FILE" "$INSTALL_DIR/$SCRIPT_NAME"
    rm -rf "$DATA_DIR"
    docker rmi "$IMG" &>/dev/null || true
    systemctl daemon-reload &>/dev/null || true

    echo -e "  ${G}[OK] Fully removed.${NC}"
    pause
}

# ---------------------------------------------------------------------------
# WireGuard protocol menu wrappers
# ---------------------------------------------------------------------------
_wg_source_module() {
    _sync_protocols_source
    if [ ! -f "$PROTOCOLS_DIR/wireguard.sh" ]; then
        echo -e "  ${R}[ERROR] protocols/wireguard.sh not found. Run this from inside${NC}"
        echo -e "  ${R}         the full simorgh-tunnel checkout at least once.${NC}"
        return 1
    fi
    # shellcheck disable=SC1090
    source "$PROTOCOLS_DIR/wireguard.sh"
}

_wg_menu_install() {
    banner
    _wg_source_module || { pause; return; }
    wg_install_core
    if [ $? -eq 0 ]; then
        local port; port="$(grep -m1 '^ListenPort' "$WG_CONF" 2>/dev/null | cut -d= -f2 | tr -d ' ')"
        _start_simorgh_multiclient "${port:-51820}"
    fi
    pause
}

_wg_menu_add() {
    banner
    _wg_source_module || { pause; return; }
    if ! docker image inspect "$IMG" &>/dev/null; then
        echo -e "  ${R}[ERROR] Simorgh core image not built yet - run 'Install Core' first.${NC}"
        pause; return
    fi
    local name; name=$(ask "Customer name")
    wg_add_customer "$name"
    pause
}

_wg_menu_list_remove() {
    banner
    _wg_source_module || { pause; return; }
    echo -e "  ${W}Current WireGuard customers:${NC}"
    wg_list_customers
    echo
    local c; c=$(ask "Remove one? Enter their name, or leave empty to go back" "")
    if [ -n "$c" ]; then
        wg_remove_customer "$c"
    fi
    pause
}

# Starts (or restarts) Simorgh in multi-client server mode, relaying to the
# given local port (a protocol daemon like WireGuard listening on
# 127.0.0.1:$1) and demultiplexing customers via /etc/simorgh/customers.json.
# This is separate from the single-customer _start_tunnel flow in
# create_tunnel() - multi-client mode has no PASSWORD/REMOTE_IP of its own.
_start_simorgh_multiclient() {
    local target_port="$1"
    if ! docker image inspect "$IMG" &>/dev/null; then
        echo -e "  ${R}[ERROR] Simorgh core image not built - run 'Install Core' first.${NC}"
        return 1
    fi
    mkdir -p /etc/simorgh
    [ -f /etc/simorgh/customers.json ] || echo "[]" > /etc/simorgh/customers.json

    docker rm -f "$CONTAINER_NAME" &>/dev/null || true
    docker run -d --name "$CONTAINER_NAME" \
        --cap-add=NET_ADMIN --cap-add=NET_RAW \
        --device /dev/net/tun:/dev/net/tun \
        --net=host --restart unless-stopped \
        -e MODE=forward -e TRANSPORT=icmp -e FORWARD_PROTO=udp \
        -e TARGET_HOST=127.0.0.1 -e "TARGET_PORT=$target_port" \
        -e CUSTOMERS_FILE=/etc/simorgh/customers.json \
        -v /etc/simorgh/customers.json:/etc/simorgh/customers.json:ro \
        "$IMG" >> "$LOG_FILE" 2>&1

    sleep 1
    if docker ps --filter "name=$CONTAINER_NAME" --filter "status=running" -q | grep -q .; then
        echo -e "  ${G}[OK] Simorgh multi-client server running, relaying to 127.0.0.1:$target_port.${NC}"
    else
        echo -e "  ${R}[ERROR] Container didn't start. Check: docker logs $CONTAINER_NAME${NC}"
    fi
}

# ---------------------------------------------------------------------------
# guided setup
# ---------------------------------------------------------------------------
#
# Shown on a fresh machine, before the menu. The menu lists capabilities; it does
# not tell you which of them you want, or that two of them have to happen in a
# particular order. Someone installing this for the first time is choosing
# between "make my own connection faster" and "run a service for other people",
# and those are different setups that the menu presents as neighbouring numbers.
#
# The ordering this enforces is the part that is easy to get wrong unaided: on a
# combined server the tunnel has to exist before the panel is pointed through it,
# because the panel's outbound is configured against a tunnel that is already up.
guided_setup() {
    banner
    echo -e "  ${BOLD}What should this server do?${NC}"
    echo
    echo -e "  ${W}1) Low-latency tunnel only${NC}"
    echo -e "     ${DIM}Iran server <-> server abroad. Lower ping for games, for you.${NC}"
    echo -e "     ${DIM}No accounts, no panel.${NC}"
    echo
    echo -e "  ${W}2) VPN panel only${NC}"
    echo -e "     ${DIM}Hand out accounts to other people: WireGuard, OpenVPN, L2TP,${NC}"
    echo -e "     ${DIM}Xray, IPsec/IKEv2, PPTP, SSTP, Cisco, MTProto and more, with${NC}"
    echo -e "     ${DIM}quotas, subscription links and resellers. Usual choice for a${NC}"
    echo -e "     ${DIM}server abroad.${NC}"
    echo
    echo -e "  ${W}3) Both — tunnel first, then the panel over it${NC}"
    echo -e "     ${DIM}Usual choice for an Iran server: customers connect to the panel${NC}"
    echo -e "     ${DIM}here, and their traffic leaves through the tunnel abroad.${NC}"
    echo
    echo -e "  ${W}4) Skip this, show me the menu${NC}"
    echo
    local c; c=$(ask "Choose" "3")

    case "$c" in
        1)
            install_core
            create_tunnel
            ;;
        2)
            install_panel
            ;;
        3)
            echo
            echo -e "  ${Y}Step 1 of 3 — building the tunnel core.${NC}"
            pause
            install_core
            echo
            echo -e "  ${Y}Step 2 of 3 — creating the tunnel.${NC}"
            echo -e "  ${DIM}Run this on BOTH servers with the same password and${NC}"
            echo -e "  ${DIM}transport: pick IRAN here and FOREIGN on the other one.${NC}"
            pause
            create_tunnel
            echo
            echo -e "  ${Y}Step 3 of 3 — installing the panel.${NC}"
            echo -e "  ${DIM}The tunnel is up, so the panel can now be pointed through${NC}"
            echo -e "  ${DIM}it. In the panel this is an OUTBOUND; set it after login.${NC}"
            pause
            install_panel
            ;;
        *)  ;;
    esac
}

# first_run reports whether this machine has neither the tunnel nor the panel
# set up yet, which is the only time the guided flow is worth interrupting for.
first_run() {
    [ ! -f "$CONF_FILE" ] && [ ! -x "$PANEL_BIN" ]
}

# ---------------------------------------------------------------------------
# main menu
# ---------------------------------------------------------------------------
# nothing_installed reports a machine this script has not set anything up on
# yet. Used to decide whether to offer the guided path before dropping someone
# into an eleven-item menu.
nothing_installed() {
    [ ! -f "$CONF_FILE" ] && [ ! -x "$PANEL_BIN" ] && ! docker image inspect "$IMAGE_NAME" >/dev/null 2>&1
}

# offer_guided runs once, on a machine with nothing on it.
#
# Someone arriving here has just pasted a curl command and does not yet know
# what a "core", an "inbound" or a "node" is in this project, so eleven numbered
# options is not a menu -- it is a quiz. Asking what they want to DO and doing it
# is the difference between the tool working and the tool being abandoned at the
# first screen.
#
# It is an OFFER, not a redirect: answering no drops straight into the full menu,
# and it never appears again once anything is installed, so it cannot become
# something an experienced operator has to dismiss on every run.
offer_guided() {
    nothing_installed || return 0
    banner
    echo -e "  ${W}Nothing is set up on this machine yet.${NC}"
    echo
    echo -e "  ${DIM}The guided setup asks what this server is for and configures the rest."
    echo -e "  Answer 'n' if you would rather pick from the full menu.${NC}"
    echo
    local ans; ans=$(ask "Run guided setup? (y/n)" "y")
    case "$ans" in
        [Nn]*) return 0 ;;
        *) guided_setup ;;
    esac
}

main_menu() {
    offer_guided
    while true; do
        banner
        echo -e "  ${DIM}────────── Start here ──────────${NC}"
        echo "  g) Guided setup — asks what this server is for"
        echo -e "  ${DIM}────────── Tunnel ──────────${NC}"
        echo "  1) Install Core (local Docker build)"
        echo "  2) Create Tunnel"
        echo "  3) Manage Tunnel"
        echo "  4) Dashboard"
        echo "  5) MTU Optimizer"
        echo "  6) Full Uninstall"
        echo -e "  ${DIM}────────── Protocol cores (multi-customer) ──────────${NC}"
        echo "  7) Install WireGuard Core"
        echo "  8) Add WireGuard Customer"
        echo "  9) List / Remove WireGuard Customers"
        echo -e "  ${DIM}────────── Panel (all protocols, users, resellers) ──────────${NC}"
        if [ -x "$PANEL_BIN" ]; then
            echo " 10) Manage Panel"
        else
            echo " 10) Install Panel"
        fi
        echo " 11) Guided setup"
        echo "  0) Exit"
        echo
        local c; c=$(ask "Choose" "0")
        case "$c" in
            1) install_core ;;
            2) create_tunnel ;;
            3) manage_tunnel ;;
            4) dashboard ;;
            5) mtu_optimizer ;;
            6) uninstall_all ;;
            7) _wg_menu_install ;;
            8) _wg_menu_add ;;
            9) _wg_menu_list_remove ;;
            10) if [ -x "$PANEL_BIN" ]; then manage_panel; else install_panel; fi ;;
            11) guided_setup ;;
            0) exit 0 ;;
        esac
    done
}

require_root

if [ "${1:-}" = "--start" ]; then
    start_from_conf
    exit 0
fi

# A fresh machine gets the "what do you want" question once. Afterwards the menu
# opens directly, since by then the operator has made that decision and being
# asked again on every run is noise.
if first_run; then
    guided_setup
fi

main_menu
