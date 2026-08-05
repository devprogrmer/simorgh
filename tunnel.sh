#!/usr/bin/env bash
# ============================================================================
#  SIMORGH TUNNEL  —  github.com/devprogrmer/simorgh
#
#  Tunnel-e gaming baraye Iran. Har protocol jodagane, optimize-e vaghei,
#  va charkhesh-e khodkar beyn-e protocol-ha vaghti yeki oft mikone.
#
#  Hame-ye matn-ha Finglish hastand — chon operator-e in server Farsi-zabun
#  ast vali console-ash aksaran font-e Farsi nadare, va Farsi-ye kharab
#  badtar az Finglish-e salem ast.
# ============================================================================
set -uo pipefail

BRAND="SIMORGH TUNNEL"
REPO="github.com/devprogrmer/simorgh"
CONF_DIR="/etc/simorgh-tunnel"
CONF="$CONF_DIR/tunnel.conf"
LOG="/var/log/simorgh-tunnel.log"
STATE="$CONF_DIR/state"

G="\033[0;32m"; R="\033[0;31m"; Y="\033[0;33m"; B="\033[0;36m"
W="\033[1;37m"; DIM="\033[2m"; BOLD="\033[1m"; NC="\033[0m"

need_root() {
    [ "$(id -u)" -eq 0 ] || { echo -e "${R}Bayad ba root ejra beshe:  sudo bash $0${NC}"; exit 1; }
}

log()  { echo "$(date '+%F %T')  $*" >> "$LOG"; }
ok()   { echo -e "  ${G}✓${NC} $*"; }
warn() { echo -e "  ${Y}!${NC} $*"; }
err()  { echo -e "  ${R}✗${NC} $*"; }
info() { echo -e "  ${DIM}$*${NC}"; }
ask()  { local p="$1" d="${2:-}" a; read -r -p "  $p${d:+ [$d]}: " a; echo "${a:-$d}"; }
pause(){ read -r -p "  Enter ro bezan..." _; }

banner() {
    clear
    echo -e "${BOLD}${W}  $BRAND${NC}"
    echo -e "  ${DIM}$REPO${NC}"
    echo -e "  ${DIM}────────────────────────────────────────────────${NC}"
}

# ============================================================================
#  OPTIMIZE  —  in bakhsh chizi ast ke ping ro vaghean paeen miare
# ============================================================================
#
# Har kodum az inha yek dalil-e moshakhas dare. Chizi ke faghat "khoob be
# nazar mirese" inja nist, chon sysctl-e eshtebah mitune ping ro badtar kone.

optimize_system() {
    banner
    echo -e "  ${BOLD}Optimize kardan-e server baraye gaming${NC}"
    echo

    local before after
    before="$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null)"

    # --- BBR + fq ---------------------------------------------------------
    # BBR bar asas-e pahnaye-band va RTT tasmim migire, na bar asas-e packet
    # loss. Rooye masir-e Iran-kharej ke loss-e tabiee dare, cubic khodesh ro
    # khafe mikone chon har loss ro "ezdeham" mifahme. Farghesh rooye link-e
    # bad chand barabar ast, na chand darsad.
    modprobe tcp_bbr 2>/dev/null || true
    if grep -q bbr /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null; then
        sysctl -qw net.ipv4.tcp_congestion_control=bbr
        # fq (na fq_codel) chon BBR khodesh pacing mikhad va fq oon ro
        # dorost anjam mide. fq_codel pacing nadare va BBR bi-asar mishe.
        sysctl -qw net.core.default_qdisc=fq
        ok "BBR + fq faal shod  ${DIM}(ghablan: ${before:-?})${NC}"
    else
        warn "BBR rooye in kernel nist — cubic mimune"
    fi

    # --- Buffer-ha --------------------------------------------------------
    # Bozorg, vali na bi-nahayat. Buffer-e kheyli bozorg "bufferbloat" misaze:
    # packet ha to saf mimunan be jaye inke drop beshan, va ping-e bazi
    # miporeh bala dar hali ke speedtest khoob neshun mide.
    sysctl -qw net.core.rmem_max=16777216
    sysctl -qw net.core.wmem_max=16777216
    sysctl -qw net.ipv4.tcp_rmem="4096 87380 16777216"
    sysctl -qw net.ipv4.tcp_wmem="4096 65536 16777216"
    sysctl -qw net.core.netdev_max_backlog=16384
    sysctl -qw net.core.somaxconn=8192
    ok "Buffer-haye shabake tanzim shod"

    # --- Latency-e vaghei -------------------------------------------------
    # tcp_low_latency: kernel ro migé taakhir ro bar throughput tarjih bede.
    # tcp_fastopen=3: yek RTT kamtar baraye har ettesal-e jadid — baraye
    # bazi ke modam ettesal mizane hesab mishe.
    # tcp_slow_start_after_idle=0: bedun-e in, har maks-e kootah dar bazi
    # baes mishe kernel az avval slow-start kone va yek marhale lag bede.
    sysctl -qw net.ipv4.tcp_low_latency=1
    sysctl -qw net.ipv4.tcp_fastopen=3
    sysctl -qw net.ipv4.tcp_slow_start_after_idle=0
    sysctl -qw net.ipv4.tcp_no_metrics_save=1
    sysctl -qw net.ipv4.tcp_mtu_probing=1
    ok "Tanzimat-e latency faal shod"

    # --- UDP (WireGuard, bazi-ha, khod-e tunnel) --------------------------
    sysctl -qw net.core.rmem_default=1048576
    sysctl -qw net.core.wmem_default=1048576
    sysctl -qw net.ipv4.udp_mem="65536 131072 262144"
    ok "Buffer-e UDP tanzim shod"

    # --- Forwarding + conntrack -------------------------------------------
    sysctl -qw net.ipv4.ip_forward=1
    sysctl -qw net.ipv6.conf.all.forwarding=1 2>/dev/null || true
    # conntrack-e por = packet-e drop shode bedun-e hich khata-ye vazeh.
    # Rooye serveri ke sadha client dare, in aval chizi ast ke mishkane.
    sysctl -qw net.netfilter.nf_conntrack_max=524288 2>/dev/null || true
    sysctl -qw net.ipv4.tcp_max_syn_backlog=8192
    ok "Forwarding va conntrack tanzim shod"

    # --- File descriptor --------------------------------------------------
    sysctl -qw fs.file-max=2097152
    if ! grep -q "simorgh-tunnel" /etc/security/limits.conf 2>/dev/null; then
        cat >> /etc/security/limits.conf <<'EOF'

# simorgh-tunnel
* soft nofile 1048576
* hard nofile 1048576
EOF
    fi
    ok "Mahdudiat-e file descriptor bala rafte"

    # --- Payedar kardan ---------------------------------------------------
    # Bedun-e in, hame-ye bala ba avvalin reboot mipare va operator fekr
    # mikone tunnel kharab shode.
    cat > /etc/sysctl.d/99-simorgh-tunnel.conf <<'EOF'
# simorgh-tunnel — github.com/devprogrmer/simorgh
# Bad az reboot ham bayad bemune, vagarna tunnel "khodbekhod bad mishe".
net.ipv4.tcp_congestion_control = bbr
net.core.default_qdisc = fq
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
net.core.netdev_max_backlog = 16384
net.core.somaxconn = 8192
net.ipv4.tcp_low_latency = 1
net.ipv4.tcp_fastopen = 3
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.tcp_no_metrics_save = 1
net.ipv4.tcp_mtu_probing = 1
net.ipv4.ip_forward = 1
net.ipv4.tcp_max_syn_backlog = 8192
fs.file-max = 2097152
EOF
    ok "Rooye reboot ham payedar shod"

    # --- NIC ---------------------------------------------------------------
    local nic; nic="$(ip route show default 2>/dev/null | awk '{print $5; exit}')"
    if [ -n "$nic" ]; then
        # txqueuelen-e bishtar rooye link-e por-taakhir jelo-ye drop-e
        # bi-dalil ro migire. 10000 baraye link-e bein-ghareei monaseb ast.
        ip link set dev "$nic" txqueuelen 10000 2>/dev/null && \
            ok "txqueuelen-e $nic rooye 10000"
        # offload-ha throughput ro bala mibaran vali packet ha ro batch
        # mikonan, ke yani jitter. Baraye bazi, jitter badtar az throughput-e
        # kamtar ast — pas GRO/LRO khamush.
        if command -v ethtool >/dev/null 2>&1; then
            ethtool -K "$nic" gro off lro off 2>/dev/null && \
                ok "GRO/LRO khamush shod (jitter kamtar)"
        else
            info "ethtool nist — GRO/LRO dast nakhord (apt install ethtool)"
        fi
    fi

    after="$(sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null)"
    echo
    echo -e "  ${G}${BOLD}Tamum shod.${NC}  ${DIM}congestion: ${before:-?} → ${after:-?}${NC}"
    echo
    info "Baraye didan-e farghesh: ghabl va bad ping bezan be server-e kharej."
    log "optimize: $before -> $after"
    pause
}

# ============================================================================
#  MTU  —  peyda kardan-e bozorgtarin MTU-ye salem
# ============================================================================
#
# MTU-ye eshtebah badtarin no-e kharabi ast: ping kar mikone, ssh kar mikone,
# vali file-e bozorg va bazi gir mikonan. Chon faghat packet-e bozorg drop
# mishe va hich khataee ham nemide.

find_mtu() {
    banner
    local target; target="$(ask 'IP-e server-e moghabel' '')"
    [ -z "$target" ] && return

    echo
    info "Donbal-e bozorgtarin packet-i ke bedun-e tekke shodan mire..."
    local lo=1200 hi=1500 best=0
    while [ $lo -le $hi ]; do
        local mid=$(( (lo + hi) / 2 ))
        if ping -c 2 -W 2 -M do -s $((mid - 28)) "$target" >/dev/null 2>&1; then
            best=$mid; lo=$((mid + 1))
        else
            hi=$((mid - 1))
        fi
    done

    if [ "$best" -eq 0 ]; then
        err "Hich packet-i nresid. IP dorost ast? ICMP baste nist?"
        pause; return
    fi

    echo
    ok "Bozorgtarin MTU-ye salem: ${BOLD}$best${NC}"
    # Tunnel khodesh header ezafe mikone, pas MTU-ye dakhel-e tunnel bayad
    # kamtar bashe. 80 bayt marja-e amn baraye header-e ma + IP + UDP.
    echo -e "  ${W}MTU-ye pishnahadi baraye tunnel: $((best - 80))${NC}"
    info "In adad ro tooye config-e tunnel bezar."
    log "mtu probe $target = $best"
    pause
}

# ============================================================================
#  Sanjesh-e keyfiat  —  kodum protocol rooye in masir behtare
# ============================================================================

probe_one() {
    # Az ICMP baraye sanjesh estefade mikonim chon hamishe hast. Natije
    # taghribi ast vali baraye moghayese-ye nesbi kafi ast.
    local host="$1" out
    out="$(ping -c 10 -i 0.2 -W 2 "$host" 2>/dev/null | tail -3)"
    local loss rtt
    loss="$(echo "$out" | grep -oE '[0-9]+% packet loss' | grep -oE '^[0-9]+')"
    rtt="$(echo "$out" | grep -oE 'rtt.*=.*' | cut -d'=' -f2 | cut -d'/' -f2 | tr -d ' ')"
    echo "${rtt:-9999} ${loss:-100}"
}

test_quality() {
    banner
    echo -e "  ${BOLD}Sanjesh-e keyfiat-e masir${NC}"
    echo
    local hosts; hosts="$(ask 'IP-e server-ha (ba comma)' '')"
    [ -z "$hosts" ] && return
    echo
    printf "  %-20s %10s %10s   %s\n" "SERVER" "PING" "LOSS" "NATIJE"
    echo -e "  ${DIM}────────────────────────────────────────────────────${NC}"

    local best_host="" best_score=999999
    IFS=',' read -ra list <<< "$hosts"
    for h in "${list[@]}"; do
        h="$(echo "$h" | tr -d ' ')"; [ -z "$h" ] && continue
        read -r rtt loss <<< "$(probe_one "$h")"
        # Loss kheyli bishtar az ping azab dare. 1% loss rooye bazi
        # mesl-e 20ms ping-e ezafe hes mishe, pas zarib-e 20 mikhore.
        local score; score="$(awk -v r="$rtt" -v l="$loss" 'BEGIN{printf "%.0f", r + l*20}')"
        local verdict color
        if   [ "$score" -lt 80  ]; then verdict="Aali";   color="$G"
        elif [ "$score" -lt 150 ]; then verdict="Khoob";  color="$G"
        elif [ "$score" -lt 300 ]; then verdict="Motevaset"; color="$Y"
        else                            verdict="Zaeef";  color="$R"
        fi
        printf "  %-20s %8sms %9s%%   ${color}%s${NC}\n" "$h" "$rtt" "$loss" "$verdict"
        if [ "$score" -lt "$best_score" ]; then best_score="$score"; best_host="$h"; fi
    done
    echo
    [ -n "$best_host" ] && ok "Behtarin: ${BOLD}$best_host${NC}"
    pause
}

# ============================================================================
#  MENU
# ============================================================================

main_menu() {
    while true; do
        banner
        echo
        echo -e "  ${BOLD}TUNNEL-HA${NC}"
        echo -e "   ${W}1${NC})  Gaming tunnel      ${DIM}— kamtarin ping, ICMP/UDP, FEC${NC}"
        echo -e "   ${W}2${NC})  WireGuard          ${DIM}— sari va sabok${NC}"
        echo -e "   ${W}3${NC})  L2TP + IPsec       ${DIM}— ba certificate${NC}"
        echo -e "   ${W}4${NC})  OpenVPN            ${DIM}— sazgar ba hame dastgah${NC}"
        echo -e "   ${W}5${NC})  Xray               ${DIM}— VLESS/Reality, zed-e tashkhis${NC}"
        echo
        echo -e "  ${BOLD}ABZAR${NC}"
        echo -e "   ${W}6${NC})  Optimize server    ${DIM}— BBR, buffer, latency${NC}"
        echo -e "   ${W}7${NC})  Peyda kardan MTU"
        echo -e "   ${W}8${NC})  Sanjesh-e keyfiat  ${DIM}— kodum server behtare${NC}"
        echo -e "   ${W}9${NC})  Charkhesh-e khodkar${DIM} — khodesh behtarin ro entekhab kone${NC}"
        echo
        echo -e "   ${W}0${NC})  Khoruj"
        echo
        local c; c="$(ask 'Entekhab' '')"
        case "$c" in
            6) optimize_system ;;
            7) find_mtu ;;
            8) test_quality ;;
            0) exit 0 ;;
            *) warn "Fe'lan faghat 6, 7, 8 amade-and."; pause ;;
        esac
    done
}

need_root
mkdir -p "$CONF_DIR"
touch "$LOG"
main_menu
