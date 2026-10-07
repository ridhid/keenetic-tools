#!/bin/sh
# keenetic-tools: awg-monitor
# AmneziaWG tunnel monitor for Keenetic / Entware. The same file is the installer:
#   sh awg-monitor.sh install [--log-dir DIR]
set -eu
PATH=/opt/sbin:/opt/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

BIN=/opt/bin/awg-monitor
CONF=/opt/etc/awg-monitor.conf
CRONTAB=/opt/etc/crontab
CRON_INIT=/opt/etc/init.d/S10cron
LOCK=/tmp/awg-monitor.lock
NODIR_FLAG=/tmp/awg-monitor.nodir
# DNS capture for "missed" (RAM only).
CAP_LOG=/tmp/awg-monitor.dns.log
CAP_PID=/tmp/awg-monitor.dns.pid
DNS_MAP=/tmp/awg-monitor.dnsmap
CT_SEEN=/tmp/awg-monitor.ctseen
INCLUDES=/tmp/awg-monitor.includes
CAP_MAX=4194304
DOH_IPS='8.8.8.8 8.8.4.4 1.1.1.1 1.0.0.1 9.9.9.9 149.112.112.112 94.140.14.14 94.140.15.15 208.67.222.222 208.67.220.220 77.88.8.8 77.88.8.1 76.76.2.0 76.76.10.0'
MARKER='# keenetic-tools: awg-monitor'
DIR_MARKER=.awg-monitor
DEFAULT_LOG_DIR=/opt/var/log/awg-monitor
EVENTS_MAX=1048576

# Defaults; $CONF overrides them.
LOG_DIR=
IFACE=opkgtun0
NDM_IFACE=OpkgTun0
SERVICE=/opt/etc/init.d/S52awg-opkgtun0
AWG_CONF=/opt/etc/amnezia/amneziawg/awg0-opkgtun0.conf
PING_TARGETS='1.1.1.1 8.8.8.8'
PING_COUNT=3
HTTP_URL=http://cp.cloudflare.com/generate_204
HTTP_EVERY=5
HANDSHAKE_STALE=180
RTT_WARN=500
CONFIG_EVERY=15
KEEP_DAYS=14
DOMAINS_WATCH=
DOMAINS_EVERY=5
DOMAIN_TIMEOUT=8
WAN_IFACE=
DNS_SERVER=127.0.0.1
DNS_CAPTURE=0
LAN_IFACE=br0
MISSED_PROBES=3
MISSED_RECHECK=24
MISSED_STALL=1

die() { echo "Ошибка: $*" >&2; exit 1; }
syslog() { logger -t awg-monitor "$*" 2>/dev/null || :; }
is_num() { case ${1:-} in ''|*[!0-9]*) return 1 ;; esac; }

usage() {
    cat <<'EOF'
Использование:
  sh awg-monitor.sh install [--log-dir DIR]  — установить или обновить
  awg-monitor uninstall [--purge]   — удалить; с --purge удалить и логи
  awg-monitor check                 — замер прямо сейчас, ничего не записывает
  awg-monitor status                — последний замер и последние события
  awg-monitor report [24h|7d]       — сводка за период (по умолчанию 24h)
  awg-monitor snapshot [метка]      — снять конфигурацию
  awg-monitor snapshots             — список снимков
  awg-monitor diff [A] [B]          — разница снимков (по умолчанию два последних)
  awg-monitor bundle [дней]         — архив логов для разбора (по умолчанию 3 дня)
  awg-monitor check-domain ДОМЕН... — проверить домены напрямую и через туннель
  awg-monitor domains [24h|7d]      — сводка проверок доменов из DOMAINS_WATCH
  awg-monitor route-check ДОМЕН     — почему домен идёт (или не идёт) через туннель
  awg-monitor missed [24h|7d]       — домены и соединения мимо туннеля без ответа (DNS_CAPTURE=1)
  awg-monitor collect               — один замер (его раз в минуту запускает cron)

Каталог логов по умолчанию — из конфига, иначе /opt/var/log/awg-monitor.
Настройки: /opt/etc/awg-monitor.conf
EOF
}

load_conf() {
    if [ -f "$CONF" ]; then
        # shellcheck disable=SC1090
        . "$CONF"
    fi
}

logdir_ok() { [ -n "$LOG_DIR" ] && [ -f "$LOG_DIR/$DIR_MARKER" ]; }

require_logdir() {
    logdir_ok || die "Каталог логов '${LOG_DIR:-не задан}' недоступен (нет маркера $DIR_MARKER). Диск подключён? Переустановите: sh awg-monitor.sh install --log-dir DIR"
}

# ---------- probes ----------

# Reads ping output, prints "loss rtt_avg".
ping_stats() {
    awk '
        /packet loss/ { for (i = 1; i <= NF; i++) if ($i ~ /%/) { l = $i; sub(/%.*/, "", l) } }
        /min\/avg\/max/ { s = $0; sub(/.*= */, "", s); split(s, a, "/"); r = a[2] }
        END { printf "%s %s\n", (l == "" ? "100" : l), (r == "" ? "-" : r) }'
}

ping_to() {
    ping -c "$PING_COUNT" -w "$((PING_COUNT + 2))" "$@" 2>&1 | ping_stats
}

read_state() {
    P_state='' P_cause='' P_since='' P_ts='' P_pid='' P_ticks='' P_rx='' P_tx='' P_ep='' P_hash=''
    [ -f "$LOG_DIR/state" ] || return 0
    # shellcheck disable=SC2034  # v is read via eval
    while IFS='=' read -r k v; do
        case $k in
            state|cause|since|ts|pid|ticks|rx|tx|ep|hash) eval "P_$k=\$v" ;;
        esac
    done < "$LOG_DIR/state"
}

write_state() {
    {
        echo "state=$STATE"
        echo "cause=$CAUSE"
        echo "since=$SINCE"
        echo "ts=$NOW"
        echo "pid=$PID"
        echo "ticks=$TICKS"
        echo "rx=$RX"
        echo "tx=$TX"
        echo "ep=$EP"
        echo "hash=$HASH"
    } > "$LOG_DIR/state.tmp"
    mv "$LOG_DIR/state.tmp" "$LOG_DIR/state"
}

# Prints counter delta, or "-" when unknown or reset (awk: counters may exceed 32 bits).
delta() {
    if is_num "$1" && is_num "$2"; then
        awk -v a="$1" -v b="$2" 'BEGIN { d = a - b; if (d < 0) print "-"; else printf "%.0f\n", d }'
    else
        echo -
    fi
}

probe() {
    STAMP=$(date '+%Y-%m-%d %H:%M:%S')
    NOW=$(date +%s)
    MIN=$((NOW / 60))

    PID=$(pgrep -f "amneziawg-go $IFACE" 2>/dev/null | head -n 1)
    RSS='' TICKS=''
    if [ -n "$PID" ] && [ -r "/proc/$PID/stat" ]; then
        RSS=$(awk '/^VmRSS:/ { print $2 }' "/proc/$PID/status" 2>/dev/null)
        TICKS=$(awk '{ print $14 + $15 }' "/proc/$PID/stat" 2>/dev/null)
    fi
    [ -n "$PID" ] || PID=-
    [ -n "$RSS" ] || RSS=-
    CPU=-
    # CPU% over the last interval, assuming USER_HZ=100.
    if [ "$PID" != - ] && [ "$PID" = "$P_pid" ] && is_num "$TICKS" && is_num "$P_ticks" &&
        is_num "$P_ts" && [ "$NOW" -gt "$P_ts" ] && [ "$TICKS" -ge "$P_ticks" ]; then
        CPU=$(((TICKS - P_ticks) / (NOW - P_ts)))
    fi

    IF_UP=0 MTU=-
    if [ -d "/sys/class/net/$IFACE" ]; then
        IF_UP=1
        MTU=$(cat "/sys/class/net/$IFACE/mtu" 2>/dev/null || echo -)
    fi

    HS='' RX='' TX='' EP=''
    if [ "$IF_UP" = 1 ] && command -v awg >/dev/null 2>&1; then
        HS=$(awg show "$IFACE" latest-handshakes 2>/dev/null | awk 'NR == 1 { print $2 }')
        read -r RX TX <<EOF
$(awg show "$IFACE" transfer 2>/dev/null | awk 'NR == 1 { print $2, $3 }')
EOF
        EP=$(awg show "$IFACE" endpoints 2>/dev/null | awk 'NR == 1 { print $2 }')
    fi
    is_num "$RX" || RX=
    is_num "$TX" || TX=
    case $EP in ''|'(none)') EP=- ;; esac
    if [ "$HS" = 0 ]; then
        HS_AGE=never
    elif is_num "$HS"; then
        HS_AGE=$((NOW - HS))
    else
        HS_AGE=-
    fi
    if [ "$PID" = "$P_pid" ]; then
        RX_D=$(delta "$RX" "$P_rx")
        TX_D=$(delta "$TX" "$P_tx")
    else
        RX_D=- TX_D=-
    fi

    TUN=
    if [ "$IF_UP" = 1 ]; then
        for t in $PING_TARGETS; do
            read -r l r <<EOF
$(ping_to -I "$IFACE" "$t")
EOF
            TUN="${TUN:+$TUN,}$l/$r"
        done
    fi
    read -r TUN_LOSS TUN_RTT TUN_BEST <<EOF
$(printf '%s\n' "$TUN" | awk -F, '
    { for (i = 1; i <= NF; i++) {
        split($i, a, "/"); n++; ls += a[1]
        if (!seen || a[1] + 0 < best) { best = a[1] + 0; seen = 1 }
        if (a[2] != "-") { rs += a[2]; rn++ } } }
    END { if (!n) { print "100 - 100"; exit }
          printf "%.0f %s %d\n", ls / n, (rn ? sprintf("%.1f", rs / rn) : "-"), best }')
EOF
    [ -n "$TUN" ] || TUN=-

    HTTP=-
    if [ -n "$HTTP_URL" ] && [ "$IF_UP" = 1 ] && command -v curl >/dev/null 2>&1; then
        if [ "$TUN_BEST" -ge 100 ] || [ "${FORCE_HTTP:-0}" = 1 ] || { [ "$HTTP_EVERY" -gt 0 ] && [ $((MIN % HTTP_EVERY)) -eq 0 ]; }; then
            HTTP=$(curl --interface "$IFACE" -s -o /dev/null -w '%{http_code}:%{time_total}' \
                --max-time 8 "$HTTP_URL" 2>/dev/null) || :
            [ -n "$HTTP" ] || HTTP=000:-
        fi
    fi

    EP_LOSS=- EP_RTT=-
    if [ "$EP" != - ]; then
        h=${EP%:*}
        h=${h#[}
        h=${h%]}
        read -r EP_LOSS EP_RTT <<EOF
$(ping_to "$h")
EOF
    fi

    LOAD=$(cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo -)
    MEM=$(awk '/^MemAvailable:/ { a = $2 } /^MemFree:/ { f = $2 } END { print (a != "" ? a : f) }' /proc/meminfo 2>/dev/null)
    CT=$(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo -)
    [ -n "$MEM" ] || MEM=-
}

classify() {
    STATE=OK CAUSE=-
    http_ok=0
    case $HTTP in 2??:*|3??:*) http_ok=1 ;; esac
    if [ "$TUN_BEST" -lt 100 ] || [ "$http_ok" = 1 ]; then
        if [ "$TUN_LOSS" -gt 0 ]; then
            STATE=DEGRADED CAUSE=packet_loss
        elif [ "$TUN_RTT" != - ] && awk -v r="$TUN_RTT" -v w="$RTT_WARN" 'BEGIN { exit !(r + 0 > w + 0) }'; then
            STATE=DEGRADED CAUSE=high_rtt
        elif [ "$HTTP" != - ] && [ "$http_ok" = 0 ]; then
            STATE=DEGRADED CAUSE=http_fail
        fi
        return 0
    fi
    STATE=DOWN
    if [ "$PID" = - ]; then
        CAUSE=process_dead
    elif [ "$IF_UP" = 0 ]; then
        CAUSE=iface_missing
    elif ! is_num "$HS_AGE"; then
        CAUSE=handshake_never
    elif [ "$HS_AGE" -gt "$HANDSHAKE_STALE" ]; then
        case $EP_LOSS in
            100|100.*) CAUSE=endpoint_unreachable ;;
            *) CAUSE=handshake_stale ;;
        esac
    else
        CAUSE=tunnel_no_traffic
    fi
}

sample_line() {
    echo "$STAMP ts=$NOW state=$STATE cause=$CAUSE hs_age=$HS_AGE rx_d=$RX_D tx_d=$TX_D" \
        "tun=$TUN tun_loss=$TUN_LOSS tun_rtt=$TUN_RTT http=$HTTP ep=$EP ep_loss=$EP_LOSS ep_rtt=$EP_RTT" \
        "pid=$PID rss_kb=$RSS cpu=$CPU mtu=$MTU load=$LOAD mem_kb=$MEM ct=$CT"
}

event() {
    line="$(date '+%Y-%m-%d %H:%M:%S') ts=$(date +%s) $*"
    echo "$line" >> "$LOG_DIR/events.log"
    syslog "$*"
}

# ---------- config snapshots ----------

sec() { printf '\n### %s\n' "$1"; }

# Config parts that define behaviour; their hash detects changes.
config_stable() {
    sec "awg config: $AWG_CONF (keys redacted)"
    if [ -f "$AWG_CONF" ]; then
        awk '{
            sub(/\r$/, ""); k = tolower($0); sub(/^[ \t]*/, "", k)
            if (k ~ /^(privatekey|presharedkey)[ \t]*=/) sub(/=.*/, "= <redacted>")
            print }' "$AWG_CONF"
    else
        echo 'missing'
    fi
    sec "service settings: $SERVICE"
    grep -E '^[[:space:]]*(OPKGTUN_[A-Z_]*|AWG_CONF|VER)=' "$SERVICE" 2>/dev/null || echo 'missing'
    sec "ndm: interface $NDM_IFACE"
    ndmc -c 'show running-config' 2>/dev/null |
        awk -v ifc="interface $NDM_IFACE" '$0 == ifc { p = 1 } p { print } p && /^!/ { exit }' || :
    sec 'firmware'
    ndmc -c 'show version' 2>/dev/null | grep -E '^[[:space:]]*(release|title|model|hw_id|arch):' || :
    sec 'packages'
    opkg list-installed 2>/dev/null | grep -E '^(amneziawg|wireguard|curl |cron )' || :
}

# Volatile context, kept for reference only.
config_info() {
    sec "awg show $IFACE"
    awg show "$IFACE" 2>&1 || :
    sec "ip addr $IFACE"
    ip addr show dev "$IFACE" 2>&1 || :
    sec 'ip rule'
    ip rule show 2>&1 || :
    sec 'ip route get'
    for t in $PING_TARGETS; do ip route get "$t" 2>&1 || :; done
    sec "ndm: show interface $NDM_IFACE"
    ndmc -c "show interface $NDM_IFACE" 2>&1 || :
}

config_hash() { config_stable | md5sum | cut -c1-32; }

list_snapshots() {
    for f in "$LOG_DIR"/snapshots/*.txt; do
        if [ -f "$f" ]; then echo "${f##*/}"; fi
    done
}

last_snapshot() { list_snapshots | tail -n 1; }

# Writes a snapshot; sets SNAP_NAME and SNAP_HASH.
snapshot_write() {
    label=$(printf '%s' "${1:-}" | tr '/ \t' '___')
    label=${label#.}
    dir="$LOG_DIR/snapshots"
    mkdir -p "$dir"
    tmp="$dir/.tmp.$$"
    config_stable > "$tmp.stable"
    SNAP_HASH=$(md5sum < "$tmp.stable" | cut -c1-32)
    SNAP_NAME="$(date +%Y-%m-%d_%H%M%S)${label:+-$label}.txt"
    {
        echo '# awg-monitor snapshot'
        echo "date: $(date '+%Y-%m-%d %H:%M:%S %Z')"
        echo "label: ${label:--}"
        echo "hash: $SNAP_HASH"
        echo
        echo '## stable'
        cat "$tmp.stable"
        echo
        echo '## info'
        config_info
    } > "$tmp"
    mv "$tmp" "$dir/$SNAP_NAME"
    rm -f "$tmp.stable"
}

# ---------- failure dump ----------

write_dump() {
    mkdir -p "$LOG_DIR/dumps"
    f="$LOG_DIR/dumps/$(date +%Y-%m-%d_%H%M%S)-$CAUSE.txt"
    {
        echo "# awg-monitor dump: $STAMP state=$STATE cause=$CAUSE"
        sec 'sample'
        sample_line
        config_info
        if [ "$EP" != - ]; then
            sec 'ip route get endpoint'
            h=${EP%:*}
            ip route get "${h#[}" 2>&1 || :
        fi
        sec 'ndm: log (tail)'
        ndmc -c 'show log' 2>&1 | tail -n 150 || :
        sec 'dmesg (tail)'
        dmesg 2>&1 | tail -n 50 || :
        sec 'top'
        top -bn1 2>&1 | head -n 25 || :
        sec 'free'
        free 2>&1 || :
        sec 'recent samples'
        tail -n 15 "$LOG_DIR/samples/${STAMP%% *}.log" 2>/dev/null || :
    } > "$f" 2>&1
    echo "dumps/${f##*/}"
}

# ---------- domains ----------

# Default-route device: the "direct" path for domain checks.
wan_iface() {
    if [ -n "$WAN_IFACE" ]; then
        echo "$WAN_IFACE"
        return 0
    fi
    ip -4 route show default 2>/dev/null |
        awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }'
}

# Strips scheme, path and port; prints the lowercased host or fails on bad input.
dom_norm() {
    h=${1#*://}
    h=${h%%/*}
    h=${h%%:*}
    case $h in ''|-*|.*|*[!A-Za-z0-9.-]*) return 1 ;; esac
    echo "$h" | tr 'A-Z' 'a-z'
}

# HTTPS request to $1 bound to device $2; prints "stage code ip seconds".
# Stage is "ok" when the server answered with any HTTP code, otherwise where it broke.
dom_probe() {
    out=$(curl -4 -s -o /dev/null --interface "$2" --max-time "$DOMAIN_TIMEOUT" \
        -w '%{http_code} %{time_connect} %{time_appconnect} %{time_total} %{remote_ip}' \
        "https://$1/" 2>/dev/null) && rc=0 || rc=$?
    printf '%s %s\n' "$rc" "$out" | awk '{
        rc = $1; code = ($2 == "" ? "000" : $2); tc = $3 + 0; ta = $4 + 0
        if (code != "000") r = "ok"
        else if (rc == 6) r = "dns"
        else if (rc == 45) r = "iface"
        else if (rc == 77) r = "ca"
        else if (rc == 7) r = "tcp"
        else if (rc == 35) r = "tls"
        else if (rc == 51 || rc == 58 || rc == 60) r = "cert"
        else if (tc == 0) r = "tcp"
        else if (ta == 0) r = "tls"
        else r = "http"
        printf "%s %s %s %s\n", r, code, ($6 == "" ? "-" : $6), ($5 == "" ? "-" : sprintf("%.2f", $5)) }'
}

# Probes domains $2... via WAN and tunnel in parallel; results go to $1/N.wan and $1/N.tun.
dom_run() {
    dir=$1
    shift
    wan=$(wan_iface)
    [ "$wan" != "$IFACE" ] || wan=
    i=0
    for d in "$@"; do
        i=$((i + 1))
        if [ -n "$wan" ]; then
            dom_probe "$d" "$wan" > "$dir/$i.wan" &
        else
            echo 'nowan 000 - -' > "$dir/$i.wan"
        fi
        dom_probe "$d" "$IFACE" > "$dir/$i.tun" &
    done
    wait
}

# Reads results of domain number $2 from dir $1 into W_* and T_*.
dom_read() {
    W_r=err W_code=000 W_ip=- W_t=- T_r=err T_code=000 T_ip=- T_t=-
    read -r W_r W_code W_ip W_t < "$1/$2.wan" || :
    read -r T_r T_code T_ip T_t < "$1/$2.tun" || :
}

dom_ok() { case $1 in ok|nowan) echo ok ;; *) echo fail ;; esac; }

dom_text() {
    case $1 in
        ok) echo "ответил, HTTP $2, $3 с" ;;
        nowan) echo 'не проверено: не найден интерфейс провайдера (WAN_IFACE в конфиге)' ;;
        dns) echo 'сбой DNS: имя не разрешилось' ;;
        tcp) echo "сбой TCP: сервер не принял соединение ($3 с)" ;;
        tls) echo "сбой TLS: рукопожатие оборвано или зависло ($3 с) — похоже на DPI" ;;
        http) echo "сбой HTTP: соединение есть, ответа нет ($3 с)" ;;
        cert) echo 'неверный сертификат — возможна подмена (заглушка провайдера)' ;;
        ca) echo 'нет корневых сертификатов: opkg install ca-bundle' ;;
        iface) echo 'интерфейс недоступен' ;;
        *) echo "ошибка проверки ($1)" ;;
    esac
}

dom_verdict() {
    w=$(dom_ok "$1")
    t=$(dom_ok "$2")
    if [ "$1" = dns ] && [ "$2" = dns ]; then
        echo 'имя не разрешается — проблема DNS (AdGuard Home / DNS роутера), а не маршрута'
    elif [ "$1" = nowan ]; then
        if [ "$t" = ok ]; then echo 'через туннель работает'; else echo 'через туннель не открывается'; fi
    elif [ "$w" = ok ] && [ "$t" = ok ]; then
        echo 'доступен обоими путями'
    elif [ "$t" = ok ]; then
        echo 'напрямую не открывается, через туннель — да: домен должен идти в туннель (проверьте список маршрутизации)'
    elif [ "$w" = ok ]; then
        echo 'через туннель не открывается, напрямую — да: проблема туннеля или его сервера'
    else
        echo 'не открывается ни напрямую, ни через туннель: сайт недоступен или блокировка глубже'
    fi
}

# Domain checks from collect; temp files live in the lock dir.
dom_collect() {
    list=
    for a in $DOMAINS_WATCH; do
        if h=$(dom_norm "$a"); then list="$list $h"; fi
    done
    [ -n "$list" ] || return 0
    dir="$LOCK/dom"
    mkdir -p "$dir" "$LOG_DIR/domains"
    # shellcheck disable=SC2086
    dom_run "$dir" $list
    st="$LOG_DIR/domains.state"
    : > "$st.tmp"
    i=0
    for d in $list; do
        i=$((i + 1))
        dom_read "$dir" "$i"
        ip=$W_ip
        [ "$ip" != - ] || ip=$T_ip
        echo "$STAMP ts=$NOW domain=$d wan=$W_r wan_code=$W_code wan_t=$W_t" \
            "tun=$T_r tun_code=$T_code tun_t=$T_t ip=$ip" >> "$LOG_DIR/domains/$day.log"
        flags="$(dom_ok "$W_r")/$(dom_ok "$T_r")"
        old=$(awk -v d="$d" '$1 == d { print $2; exit }' "$st" 2>/dev/null || :)
        if [ "$flags" != "${old:-ok/ok}" ]; then
            event "domain_changed domain=$d wan=$W_r tun=$T_r"
        fi
        echo "$d $flags" >> "$st.tmp"
    done
    mv "$st.tmp" "$st"
}

# ---------- domain routing (Keenetic object-group fqdn) ----------

tun_addr() {
    ip -4 addr show dev "$IFACE" 2>/dev/null | awk '$1 == "inet" { sub(/\/.*/, "", $2); print $2; exit }'
}

# Prints "group include" for groups routed to $NDM_IFACE whose include covers domain $1.
# Last line is "groups G1 G2..." with all such groups.
dom_cover() {
    ndmc -c 'show running-config' 2>/dev/null | awk -v d="$1" -v ifc="$NDM_IFACE" '
        { sub(/\r$/, "") }
        /^[^ !]/ { ing = 0; dp = 0 }
        /^object-group fqdn / { g = $3; ing = 1; next }
        /^dns-proxy$/ { dp = 1; next }
        ing && $1 == "include" { inc[g] = inc[g] " " tolower($2) }
        dp && $1 == "route" && $2 == "object-group" && $4 == ifc { r[$3] = 1 }
        END {
            for (g in r) {
                gl = gl " " g
                n = split(inc[g], a, " ")
                for (i = 1; i <= n; i++)
                    if (d == a[i] || (length(d) > length(a[i]) && substr(d, length(d) - length(a[i])) == "." a[i])) print g, a[i]
            }
            print "groups" gl
        }'
}

# Resolves $1 via the router DNS (this also feeds Keenetic routing); prints addresses.
dom_resolve() {
    nslookup "$1" "$DNS_SERVER" 2>/dev/null | awk '
        /^Name:/ { a = 1; next }
        a && /^Address/ { sub(/^Address[ 0-9]*:[ \t]*/, ""); x = $1; if (x ~ /^[0-9.]+$/ || x ~ /:/) print x }'
}

# Conntrack flows from LAN to IPs in $1 (space-separated, empty = all public IPv4).
# Prints "client dst proto dport via replied orig_bytes reply_bytes state".
# Optional $3 = LAN CIDR: only clients from it. Column 10 is the client port.
ct_flows() {
    awk -v ips=" $1 " -v tun="$2" -v lan="${3:-}" '
        function num(a,   x) { split(a, x, "."); return ((x[1] * 256 + x[2]) * 256 + x[3]) * 256 + x[4] }
        BEGIN {
            if (lan != "") { split(lan, l, "/"); bits = (l[2] == "" ? 32 : l[2]); div = 2 ^ (32 - bits); net = int(num(l[1]) / div) }
        }
        function priv(a) { return a ~ /^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|127\.|169\.254\.|22[4-9]\.|2[3-5][0-9]\.|0\.)/ }
        $1 == "ipv4" {
            split("", o); split("", r); st = "-"; unr = 0; side = 0
            if ($3 == "tcp") st = $6
            for (i = 4; i <= NF; i++) {
                if ($i == "[UNREPLIED]") { unr = 1; continue }
                p = index($i, "="); if (p < 2) continue
                k = substr($i, 1, p - 1); v = substr($i, p + 1)
                if (k == "src") side++
                if (side == 1 && !(k in o)) o[k] = v
                else if (side == 2 && !(k in r)) r[k] = v
            }
            if (!priv(o["src"]) || priv(o["dst"])) next
            if (lan != "" && int(num(o["src"]) / div) != net) next
            if (ips != "  " && index(ips, " " o["dst"] " ") == 0) next
            via = (tun != "" && r["dst"] == tun) ? "tun" : "wan"
            print o["src"], o["dst"], $3, (o["dport"] == "" ? "-" : o["dport"]), via, (unr ? "no" : "yes"),
                (o["bytes"] == "" ? "-" : o["bytes"]), (r["bytes"] == "" ? "-" : r["bytes"]), st,
                (o["sport"] == "" ? "-" : o["sport"])
        }' /proc/net/nf_conntrack 2>/dev/null
}

# Counts LAN IPv6 flows to global addresses (they bypass an IPv4-only tunnel).
ct_v6_count() {
    awk '$1 == "ipv6" {
            for (i = 4; i <= NF; i++) if ($i ~ /^dst=/) { d = substr($i, 5); break }
            if (d !~ /^f[ef]/ && d !~ /^0000/) n++
        } END { print n + 0 }' /proc/net/nf_conntrack 2>/dev/null
}

cmd_route_check() {
    [ "$#" -eq 1 ] || die 'Укажите один домен: awg-monitor route-check example.com'
    d=$(dom_norm "$1") || die "Некорректный домен: '$1'."
    command -v ndmc >/dev/null 2>&1 || die 'ndmc не найден — команда работает только на Keenetic.'
    cover=$(dom_cover "$d")
    groups=$(printf '%s\n' "$cover" | sed -n 's/^groups *//p')
    hit=$(printf '%s\n' "$cover" | grep -v '^groups' | head -n 1 || :)
    echo "Домен: $d"
    if [ -z "$groups" ]; then
        echo "1. Список: не найдено групп с маршрутом в $NDM_IFACE (dns-proxy route object-group ... $NDM_IFACE)."
    elif [ -n "$hit" ]; then
        echo "1. Список: покрыт — группа ${hit%% *}, запись include ${hit#* }"
    else
        echo "1. Список: НЕ покрыт ни одной группой ($groups) — трафик пойдёт мимо туннеля."
    fi

    addrs=$(dom_resolve "$d" | tr '\n' ' ')
    v4=$(printf '%s\n' $addrs | grep -E '^[0-9.]+$' | tr '\n' ' ' || :)
    v6=$(printf '%s\n' $addrs | grep : | tr '\n' ' ' || :)
    if [ -n "$v4$v6" ]; then
        echo "2. DNS роутера ($DNS_SERVER): IPv4: ${v4:-нет}; IPv6: ${v6:-нет}"
    else
        echo "2. DNS роутера ($DNS_SERVER): имя не разрешилось."
    fi

    missing=
    if [ -n "$hit" ] && [ -n "$v4" ]; then
        g=${hit%% *}
        # Give ndnproxy a moment to apply the answer to the runtime list.
        sleep 1
        listed=$(ndmc -c "show object-group fqdn $g" 2>/dev/null | awk '
            $1 == "fqdn:" { f = $2 } $1 == "address:" { print $2, f }')
        for ip in $v4; do
            f=$(printf '%s\n' "$listed" | awk -v ip="$ip" '$1 == ip { print $2; exit }')
            if [ -n "$f" ]; then
                echo "3. $ip — в runtime-списке группы $g (как $f)"
            else
                echo "3. $ip — НЕТ в runtime-списке группы $g"
                missing="$missing $ip"
            fi
        done
    fi

    tun=$(tun_addr)
    flows=
    [ -z "$v4" ] || flows=$(ct_flows "$v4" "$tun")
    if [ -z "$v4" ]; then
        echo '4. Соединения: нечего проверять — нет IPv4-адресов.'
    elif [ -n "$flows" ]; then
        echo "4. Текущие соединения устройств к этим IP (адрес туннеля ${tun:-?}):"
        printf '%s\n' "$flows" | awk '{
            printf "   %s -> %s %s/%s через %s, ответ %s, байт %s/%s%s\n", $1, $2, $3, $4,
                ($5 == "tun" ? "ТУННЕЛЬ" : "ПРОВАЙДЕРА"), ($6 == "yes" ? "есть" : "НЕТ"), $7, $8, ($9 == "-" ? "" : ", " $9) }'
    else
        echo '4. Текущих соединений устройств к этим IP нет (откройте сайт на устройстве и повторите).'
    fi
    wan_n=$(printf '%s\n' "$flows" | awk '$5 == "wan"' | grep -c . || :)

    if command -v curl >/dev/null 2>&1; then
        WORK=$(mktemp -d /tmp/awg-monitor-dom.XXXXXX)
        trap 'rm -rf "$WORK"' 0
        dom_run "$WORK" "$d"
        dom_read "$WORK" 1
        echo "5. Проверка с роутера: напрямую — $(dom_text "$W_r" "$W_code" "$W_t"); туннель — $(dom_text "$T_r" "$T_code" "$T_t")"
    fi

    v6n=$(ct_v6_count)
    echo
    echo 'Итог:'
    if [ -n "$groups" ] && [ -z "$hit" ]; then
        base=$(echo "$d" | awk -F. 'NF > 2 { print $(NF - 1) "." $NF; next } { print }')
        if [ "$base" = "$d" ]; then
            echo "  Домен не в списке. Добавьте в группу: include $d"
        else
            echo "  Домен не в списке. Добавьте в группу: include $base (или $d)."
        fi
    fi
    [ -z "$missing" ] || echo "  IP$missing не попали в список: проверьте, что устройство спрашивает DNS роутера."
    if [ -n "$hit" ] && [ "$wan_n" -gt 0 ]; then
        echo '  Домен в списке, но соединения идут мимо туннеля: устройство получило IP раньше (кэш DNS —'
        echo '  переподключите устройство / ipconfig /flushdns) или берёт его из своего DNS (DoH, Private DNS).'
    fi
    if [ -n "$v6" ] && [ "$v6n" -gt 0 ]; then
        echo "  У домена есть IPv6, а в сети $v6n IPv6-соединений в интернет: туннель только IPv4,"
        echo '  такой трафик идёт мимо. Отключите IPv6 в домашней сети или для этих устройств.'
    fi
    echo '  Шаг 4 видит только IP, полученные с роутера сейчас; у CDN устройство могло получить другие.'
}

# ---------- missed: flows that bypass the tunnel ----------

cap_running() {
    p=$(cat "$CAP_PID" 2>/dev/null || :)
    [ -n "$p" ] && kill -0 "$p" 2>/dev/null && grep -q tcpdump "/proc/$p/cmdline" 2>/dev/null
}

cap_stop() {
    if cap_running; then kill "$(cat "$CAP_PID")" 2>/dev/null || :; fi
    rm -f "$CAP_PID" "$CAP_LOG" "$CAP_LOG.work"
}

# Keeps tcpdump capturing DNS answers to LAN clients. -c bounds RAM if collect stops.
cap_ensure() {
    if cap_running; then
        if [ "$(wc -c < "$CAP_LOG" 2>/dev/null || echo 0)" -le "$CAP_MAX" ]; then return 0; fi
        cap_stop
        syslog 'захват DNS: файл вырос сверх лимита, перезапуск'
    fi
    # Append mode: after truncation tcpdump keeps writing from the start.
    tcpdump -l -n -t -vv -s 1500 -c 20000 -i "$LAN_IFACE" 'udp src port 53' \
        >> "$CAP_LOG" 2>/dev/null < /dev/null &
    echo $! > "$CAP_PID"
}

lan_cidr() {
    ip -4 addr show dev "$LAN_IFACE" 2>/dev/null | awk '$1 == "inet" { print $2; exit }'
}

# Tunnel group includes, one per line; refreshed every 10 minutes.
includes_refresh() {
    if [ -s "$INCLUDES" ] && [ $((MIN % 10)) -ne 0 ]; then return 0; fi
    ndmc -c 'show running-config' 2>/dev/null | awk -v ifc="$NDM_IFACE" '
        { sub(/\r$/, "") }
        /^[^ !]/ { ing = 0; dp = 0 }
        /^object-group fqdn / { g = $3; ing = 1; next }
        /^dns-proxy$/ { dp = 1; next }
        ing && $1 == "include" { inc[g] = inc[g] " " tolower($2) }
        dp && $1 == "route" && $2 == "object-group" && $4 == ifc { r[$3] = 1 }
        END { for (g in r) { n = split(inc[g], a, " "); for (i = 1; i <= n; i++) print a[i] } }' > "$INCLUDES.tmp" || :
    mv "$INCLUDES.tmp" "$INCLUDES"
}

# Moves captured answers into $DNS_MAP: "ts ip domain client", latest per IP, last 6 hours.
dns_harvest() {
    touch "$DNS_MAP"
    [ -s "$CAP_LOG" ] || return 0
    cp "$CAP_LOG" "$CAP_LOG.work"
    : > "$CAP_LOG"
    {
        cat "$DNS_MAP"
        awk -v now="$NOW" '/ q: / {
            c = ""; qn = ""
            for (i = 1; i <= NF; i++) {
                if ($i == ">" && c == "") { c = $(i + 1); sub(/:$/, "", c); sub(/\.[0-9]+$/, "", c) }
                if ($i == "q:") { if ($(i + 1) != "A?") next; qn = tolower($(i + 2)); sub(/\.$/, "", qn); q = i + 3; break }
            }
            if (qn == "") next
            for (j = q; j < NF; j++) if ($j == "A") { a = $(j + 1); sub(/,$/, "", a); print now, a, qn, c }
        }' "$CAP_LOG.work"
    } | awk -v from="$((NOW - 21600))" '$1 >= from { t[$2] = $1; d[$2] = $3; c[$2] = $4 }
        END { for (k in t) print t[k], k, d[k], c[k] }' > "$DNS_MAP.tmp"
    mv "$DNS_MAP.tmp" "$DNS_MAP"
    rm -f "$CAP_LOG.work"
}

# One minute of conntrack: logs new failed/suspicious LAN flows, then probes new domains.
missed_collect() {
    if ! command -v tcpdump >/dev/null 2>&1; then
        if [ ! -f "$CAP_PID.missing" ]; then
            : > "$CAP_PID.missing"
            syslog 'DNS_CAPTURE=1, но tcpdump не установлен: opkg install tcpdump'
        fi
        return 0
    fi
    rm -f "$CAP_PID.missing"
    cap_ensure
    dns_harvest
    includes_refresh
    touch "$CT_SEEN"
    mkdir -p "$LOG_DIR/missed"
    out="$LOG_DIR/missed/$day.log"
    flows="$LOCK/flows"
    ct_flows '' "$(tun_addr)" "$(lan_cidr)" > "$flows"
    awk -v stamp="$STAMP" -v now="$NOW" -v stall="$MISSED_STALL" -v dohs=" $DOH_IPS " \
        -v seen_out="$CT_SEEN.tmp" -v new_out="$LOCK/new" '
        function covered(d,   p) {
            while (d != "") { if (d in inc) return "yes"; p = index(d, "."); if (!p) break; d = substr(d, p + 1) }
            return "no"
        }
        FILENAME == ARGV[1] { dom[$2] = $3; next }
        FILENAME == ARGV[2] { inc[$1] = 1; next }
        FILENAME == ARGV[3] { seen[$1] = 1; next }
        {
            stage = ""
            if ($4 == 853) stage = "dot"
            else if ($4 == 443 && index(dohs, " " $2 " ")) stage = "doh"
            else if ($6 == "no") stage = ($3 == "udp" ? "noreply_udp" : "noreply")
            else if (stall == 1 && $3 == "tcp" && $9 == "ESTABLISHED" && $4 == 443 && $7 + 0 >= 600 && $8 + 0 < 400) stage = "stall"
            if (stage == "") next
            key = $1 ":" $10 ">" $2 ":" $4 "/" $3
            print key > seen_out
            if (key in seen) next
            d = ($2 in dom) ? dom[$2] : "-"
            cov = (d == "-") ? "-" : covered(d)
            print stamp " ts=" now " kind=flow client=" $1 " ip=" $2 " proto=" $3 " dport=" $4 " via=" $5 \
                " stage=" stage " domain=" d " covered=" cov
            if ($5 == "wan" && cov == "no" && stage != "doh" && stage != "dot") print d > new_out
        }' "$DNS_MAP" "$INCLUDES" "$CT_SEEN" "$flows" >> "$out"
    touch "$CT_SEEN.tmp"
    mv "$CT_SEEN.tmp" "$CT_SEEN"

    [ -s "$LOCK/new" ] && is_num "$MISSED_PROBES" && [ "$MISSED_PROBES" -gt 0 ] || return 0
    cache="$LOG_DIR/missed.cache"
    touch "$cache"
    list=$(sort -u "$LOCK/new" | awk -v from="$((NOW - MISSED_RECHECK * 3600))" -v max="$MISSED_PROBES" '
        FILENAME == ARGV[1] { if ($2 >= from) fresh[$1] = 1; next }
        !($1 in fresh) && n < max { print; n++ }' "$cache" -)
    [ -n "$list" ] || return 0
    mkdir -p "$LOCK/probe"
    # shellcheck disable=SC2086
    dom_run "$LOCK/probe" $list
    i=0
    for d in $list; do
        i=$((i + 1))
        dom_read "$LOCK/probe" "$i"
        echo "$STAMP ts=$NOW kind=probe domain=$d wan=$W_r wan_t=$W_t tun=$T_r tun_t=$T_t" >> "$out"
        echo "$d $NOW $W_r $T_r" >> "$cache"
        if [ "$(dom_ok "$W_r")" = fail ] && [ "$T_r" = ok ]; then
            event "missed_candidate domain=$d wan=$W_r tun=ok"
        fi
    done
    # Keep only the newest entry per domain.
    awk '{ l[$1] = $0 } END { for (k in l) print l[k] }' "$cache" > "$cache.tmp"
    mv "$cache.tmp" "$cache"
}

cmd_missed() {
    require_logdir
    parse_period "${1:-}"
    set -- "$LOG_DIR"/missed/*.log
    if [ ! -e "$1" ]; then
        [ "$DNS_CAPTURE" = 1 ] || die 'Сбор выключен. Включите: opkg install tcpdump, затем DNS_CAPTURE=1 в /opt/etc/awg-monitor.conf.'
        die 'Данных пока нет — подождите несколько минут.'
    fi
    cat "$@" | awk -v from="$from" -v to="$now" -v period="$period" '
        function kv(   i, p) {
            split("", f)
            for (i = 3; i <= NF; i++) { p = index($i, "="); if (p > 1) f[substr($i, 1, p - 1)] = substr($i, p + 1) }
        }
        function add(arr, k, v) { if (index(" " arr[k] " ", " " v " ") == 0) arr[k] = arr[k] (arr[k] == "" ? "" : " ") v }
        function nwords(s,   x) { return split(s, x, " ") }
        function base(d,   x, n) { n = split(d, x, "."); return n > 2 ? x[n - 1] "." x[n] : d }
        # Prints keys of cnt sorted by count, at most max.
        function top(cnt, max, title, extra,   k, i, j, n, ks, t, shown) {
            n = 0; for (k in cnt) ks[++n] = k
            for (i = 2; i <= n; i++) { t = ks[i]; for (j = i - 1; j > 0 && cnt[ks[j]] < cnt[t]; j--) ks[j + 1] = ks[j]; ks[j + 1] = t }
            if (!n) return 0
            print "\n" title
            for (i = 1; i <= n && i <= max; i++) printf "  %-40s соединений %d, устройства: %s%s\n", ks[i], cnt[ks[i]], cl[ks[i]], (extra ? extra_of(ks[i]) : "")
            if (n > max) printf "  … и ещё %d\n", n - max
            return n
        }
        function extra_of(k) { return (k in pr) ? "; проверка: напрямую " pr[k] ", туннель " pt[k] : "" }
        {
            kv(); ts = f["ts"] + 0
            if (ts < from || ts > to) next
            if (f["kind"] == "probe") { pr[f["domain"]] = f["wan"]; pt[f["domain"]] = f["tun"]; next }
            if (f["kind"] != "flow") next
            st = f["stage"]; d = f["domain"]; c = f["client"]; v = f["via"]
            if (st == "doh" || st == "dot") { dc[c]++; add(dip, c, f["ip"] ":" f["dport"]); next }
            if (v == "tun") { k = (d == "-" ? f["ip"] : d); tc[k]++; add(cl, k, c); next }
            if (d == "-") { k = f["ip"] ":" f["dport"] "/" f["proto"]; nd[k]++; add(cl, k, c); next }
            add(cl, d, c); add(stg, d, st)
            if (f["covered"] == "yes") cv[d]++; else un[d]++
        }
        END {
            print "Период " period "."
            n = 0
            for (d in un) if ((d in pr) && pr[d] != "ok" && pr[d] != "nowan" && pt[d] == "ok") { cand[d] = un[d]; n++; add(bases, "x", base(d)) }
            any = n
            top(cand, 50, "1. Не в списке, напрямую не открываются, через туннель — да (добавить в список):", 0)
            if (n) {
                print "   Предлагается добавить в группу:"
                k = split(bases["x"], b, " "); for (i = 1; i <= k; i++) print "     include " b[i]
            }
            for (d in un) if (!(d in cand)) rest[d] = un[d]
            any += top(rest, 30, "2. Не в списке, соединения мимо туннеля без ответа (не подтверждено проверкой):", 1)
            any += top(cv, 30, "3. В списке, но соединения пошли мимо туннеля (кэш DNS на устройстве, DoH, IPv6):", 0)
            any += top(nd, 20, "4. Без ответа и без запроса к DNS роутера (DoH на устройстве или вшитые IP):", 0)
            for (c in dc) { printf "%s", (hdr++ ? "" : "\n5. Устройства со своим DNS (DoH/DoT) — их домены роутер не видит:\n"); printf "  %s — соединений %d: %s\n", c, dc[c], dip[c] }
            any += top(tc, 10, "6. Через туннель без ответа (проблема туннеля или сервера):", 0)
            if (!any && !hdr) print "Соединений мимо туннеля без ответа не найдено."
        }'
    if [ "$DNS_CAPTURE" = 1 ] && ! cap_running; then
        echo
        echo 'Внимание: захват DNS сейчас не запущен (появится в течение минуты; нужен tcpdump).'
    fi
}

# ---------- commands ----------

acquire_lock() {
    if mkdir "$LOCK" 2>/dev/null; then
        echo $$ > "$LOCK/pid"
        return 0
    fi
    old=$(cat "$LOCK/pid" 2>/dev/null || :)
    if [ -n "$old" ] && kill -0 "$old" 2>/dev/null; then
        return 1
    fi
    rm -rf "$LOCK"
    mkdir "$LOCK" 2>/dev/null || return 1
    echo $$ > "$LOCK/pid"
}

cmd_collect() {
    if ! logdir_ok; then
        # Disk missing: do not write into an empty mount point; warn once.
        cap_stop
        if [ ! -f "$NODIR_FLAG" ]; then
            : > "$NODIR_FLAG"
            syslog "каталог логов '${LOG_DIR:-не задан}' недоступен, замеры не пишутся"
        fi
        exit 0
    fi
    rm -f "$NODIR_FLAG"
    acquire_lock || exit 0
    trap 'rm -rf "$LOCK"' 0
    read_state
    probe
    classify
    day=${STAMP%% *}
    mkdir -p "$LOG_DIR/samples"
    sample_line >> "$LOG_DIR/samples/$day.log"

    SINCE=$P_since
    if [ "$STATE" != "$P_state" ]; then
        SINCE=$NOW
        msg="state ${P_state:-start}->$STATE cause=$CAUSE"
        if [ "$P_state" = DOWN ] && is_num "$P_since"; then
            msg="$msg down_for=$((NOW - P_since))s"
        fi
        if [ "$STATE" = DOWN ]; then
            msg="$msg dump=$(write_dump)"
        fi
        event "$msg"
    fi
    if [ -n "$P_pid" ] && [ "$PID" != - ] && [ "$PID" != "$P_pid" ]; then
        event "process_restarted old=$P_pid new=$PID"
    fi
    if [ -n "$P_ep" ] && [ "$P_ep" != - ] && [ "$EP" != - ] && [ "$EP" != "$P_ep" ]; then
        event "endpoint_changed old=$P_ep new=$EP"
    fi

    HASH=$P_hash
    if [ -z "$HASH" ] || { [ "$CONFIG_EVERY" -gt 0 ] && [ $((MIN % CONFIG_EVERY)) -eq 0 ]; }; then
        base=$HASH
        if [ -z "$base" ] && last=$(last_snapshot) && [ -n "$last" ]; then
            base=$(sed -n 's/^hash: //p' "$LOG_DIR/snapshots/$last")
        fi
        HASH=$(config_hash)
        if [ "$HASH" != "$base" ]; then
            snapshot_write auto
            HASH=$SNAP_HASH
            event "config_changed snapshot=$SNAP_NAME"
        fi
    fi

    if [ "$DNS_CAPTURE" = 1 ]; then
        missed_collect
    elif [ -f "$CAP_PID" ]; then
        cap_stop
    fi

    if [ -n "$DOMAINS_WATCH" ] && is_num "$DOMAINS_EVERY" && [ "$DOMAINS_EVERY" -gt 0 ] &&
        [ $((MIN % DOMAINS_EVERY)) -eq 0 ] && command -v curl >/dev/null 2>&1; then
        dom_collect
    fi

    # Housekeeping once a day.
    if ! is_num "$P_ts" || [ "$(date -d "@$P_ts" +%Y-%m-%d 2>/dev/null)" != "$day" ]; then
        for d in samples domains missed dumps bundles; do
            [ -d "$LOG_DIR/$d" ] &&
                find "$LOG_DIR/$d" -type f -mtime +"$KEEP_DAYS" -exec rm -f {} \; 2>/dev/null || :
        done
    fi
    if [ -f "$LOG_DIR/events.log" ] && [ "$(wc -c < "$LOG_DIR/events.log")" -gt "$EVENTS_MAX" ]; then
        mv "$LOG_DIR/events.log" "$LOG_DIR/events.log.1"
    fi
    write_state
}

cmd_check() {
    if logdir_ok; then read_state; else
        P_state='' P_cause='' P_since='' P_ts='' P_pid='' P_ticks='' P_rx='' P_tx='' P_ep='' P_hash=''
    fi
    FORCE_HTTP=1
    probe
    classify
    if [ "$CAUSE" = - ]; then echo "Состояние: $STATE"; else echo "Состояние: $STATE ($CAUSE)"; fi
    if [ "$IF_UP" = 1 ]; then
        echo "Интерфейс $IFACE: есть, MTU $MTU"
    else
        echo "Интерфейс $IFACE: НЕТ"
    fi
    echo "amneziawg-go: PID $PID, RSS ${RSS} КБ, CPU ${CPU}%"
    echo "Handshake: $HS_AGE с назад (порог $HANDSHAKE_STALE с)"
    echo "Ping через туннель (потери%/RTT мс по целям $PING_TARGETS): $TUN"
    echo "HTTP через туннель (код:время): $HTTP"
    echo "Endpoint $EP мимо туннеля: потери ${EP_LOSS}%, RTT $EP_RTT мс"
    echo "Роутер: load $LOAD, свободно памяти $MEM КБ, conntrack $CT"
    echo
    sample_line
}

cmd_status() {
    require_logdir
    echo "Каталог логов: $LOG_DIR"
    if grep -Fq "$MARKER" "$CRONTAB" 2>/dev/null; then
        echo 'Сбор по cron: включён'
    else
        echo 'Сбор по cron: НЕ найден в crontab'
    fi
    read_state
    if [ -n "$P_state" ]; then
        echo "Текущее состояние: $P_state (причина: $P_cause), с $(date -d "@$P_since" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo "$P_since")"
    fi
    # shellcheck disable=SC2012
    last=$(ls "$LOG_DIR/samples" 2>/dev/null | tail -n 1)
    if [ -n "$last" ]; then
        echo
        echo 'Последний замер:'
        tail -n 1 "$LOG_DIR/samples/$last" | awk '{ print "  " $1 " " $2; for (i = 3; i <= NF; i++) print "  " $i }'
    fi
    if [ -f "$LOG_DIR/events.log" ]; then
        echo
        echo 'Последние события:'
        tail -n 10 "$LOG_DIR/events.log" | sed 's/^/  /'
    fi
}

# Parses period $1 (24h, 7d); sets period, now, from.
parse_period() {
    period=${1:-24h}
    case $period in
        *h) n=${period%h} mult=3600 ;;
        *d) n=${period%d} mult=86400 ;;
        *) die "Период задаётся как 24h или 7d." ;;
    esac
    is_num "$n" && [ "$n" -gt 0 ] || die "Период задаётся как 24h или 7d."
    now=$(date +%s)
    from=$((now - n * mult))
}

cmd_report() {
    require_logdir
    parse_period "${1:-}"
    set -- "$LOG_DIR"/samples/*.log
    [ -e "$1" ] || die "Замеров пока нет. Первый появится в течение минуты после установки."
    cat "$@" | awk -v from="$from" -v to="$now" -v period="$period" '
        function kv(   i, p) {
            split("", f)
            for (i = 3; i <= NF; i++) { p = index($i, "="); if (p > 1) f[substr($i, 1, p - 1)] = substr($i, p + 1) }
        }
        function close_down(endstamp, endts) {
            no++; os[no] = ds; oe[no] = endstamp; od[no] = endts - dts; oc[no] = dc; indown = 0
        }
        function num(v) { return v != "" && v != "-" }
        {
            kv(); ts = f["ts"] + 0
            if (ts < from || ts > to) next
            n++; st = f["state"]; cnt[st]++
            if (st != "OK") why[st ": " f["cause"]]++
            stamp = $1 " " substr($2, 1, 5)
            if (n == 1) first = stamp
            if (pts && ts - pts > 150) { gaps++; gapsec += ts - pts - 60 }
            if (st == "DOWN" && !indown) { indown = 1; ds = stamp; dts = ts; dc = f["cause"] }
            else if (st != "DOWN" && indown) close_down(stamp, ts)
            p = f["pid"]
            if (num(p)) {
                if (lp != "" && p != lp) { rst++; m = substr($2, 4, 2); if (m == "01" || m == "02" || m == "03") rsched++ }
                lp = p
            }
            if (num(f["tun_rtt"])) { rtt += f["tun_rtt"]; rttn++; if (f["tun_rtt"] + 0 > rttmax) rttmax = f["tun_rtt"] + 0 }
            if (num(f["tun_loss"])) { loss += f["tun_loss"]; lossn++ }
            r = f["rss_kb"]
            if (num(r)) { if (rf == "") rf = r; rl = r; if (r + 0 > rmax) rmax = r + 0 }
            c = f["cpu"]
            if (num(c)) { cpus += c; cpun++; if (c + 0 > cpumax) cpumax = c + 0 }
            pts = ts; last = stamp
        }
        END {
            if (!n) { print "За период " period " замеров нет."; exit }
            if (indown) close_down(last " (продолжается)", pts + 60)
            printf "Период %s: %s — %s, замеров %d\n", period, first, last, n
            printf "OK %.1f%% | DEGRADED %.1f%% | DOWN %.1f%%\n", 100 * cnt["OK"] / n, 100 * cnt["DEGRADED"] / n, 100 * cnt["DOWN"] / n
            if (gaps) printf "Пропуски данных: %d (~%d мин) — роутер, диск или cron не работали\n", gaps, gapsec / 60
            printf "Туннель: потери в среднем %.1f%%, RTT средний %s мс, максимальный %s мс\n",
                (lossn ? loss / lossn : 0), (rttn ? sprintf("%.1f", rtt / rttn) : "-"), (rttn ? sprintf("%.1f", rttmax) : "-")
            printf "amneziawg-go: перезапусков %d (в минуты :01–:03 — %d, похоже на плановый рестарт)\n", rst, rsched
            if (rf != "") printf "Память amneziawg-go: %s → %s КБ (максимум %d)\n", rf, rl, rmax
            if (cpun) printf "CPU amneziawg-go: в среднем %.0f%%, максимум %d%%\n", cpus / cpun, cpumax
            hdr = 0
            for (k in why) { if (!hdr) { print "\nНе-OK замеры по причинам (1 замер ≈ 1 мин):"; hdr = 1 } printf "  %-34s %d\n", k, why[k] }
            if (no) {
                print "\nСбои (DOWN), последние 30:"
                for (i = (no > 30 ? no - 29 : 1); i <= no; i++) {
                    mins = int((od[i] + 30) / 60); if (mins < 1) mins = 1
                    printf "  %s — %s  %d мин  %s\n", os[i], oe[i], mins, oc[i]
                }
            } else print "\nСбоев (DOWN) не было."
        }'
    if [ -f "$LOG_DIR/events.log" ]; then
        awk -v from="$from" '
            { t = $3; sub(/^ts=/, "", t); if (t + 0 < from) next }
            $4 == "endpoint_changed" { ep++; el = el "\n  " $1 " " $2 " " $5 " " $6 }
            $4 == "config_changed" { cf++; cl = cl "\n  " $1 " " $2 " " $5 }
            END {
                if (ep) printf "\nСмена Endpoint: %d%s\n", ep, el
                if (cf) printf "\nИзменения конфигурации: %d%s\n(сравнить: awg-monitor diff)\n", cf, cl
            }' "$LOG_DIR/events.log" 2>/dev/null
    fi
}

cmd_check_domain() {
    [ "$#" -gt 0 ] || die 'Укажите домен: awg-monitor check-domain example.com'
    command -v curl >/dev/null 2>&1 || die 'Нужен curl: opkg install curl ca-bundle'
    list=
    for a in "$@"; do
        h=$(dom_norm "$a") || die "Некорректный домен: '$a'."
        list="$list $h"
    done
    WORK=$(mktemp -d /tmp/awg-monitor-dom.XXXXXX)
    trap 'rm -rf "$WORK"' 0
    # shellcheck disable=SC2086
    dom_run "$WORK" $list
    echo "Напрямую — через ${wan:-?}, туннель — через $IFACE; таймаут $DOMAIN_TIMEOUT с."
    i=0
    for d in $list; do
        i=$((i + 1))
        dom_read "$WORK" "$i"
        echo
        ip=$W_ip
        [ "$ip" != - ] || ip=$T_ip
        if [ "$ip" != - ]; then echo "$d ($ip)"; else echo "$d"; fi
        echo "  напрямую: $(dom_text "$W_r" "$W_code" "$W_t")"
        echo "  туннель:  $(dom_text "$T_r" "$T_code" "$T_t")"
        echo "  итог: $(dom_verdict "$W_r" "$T_r")"
    done
}

cmd_domains() {
    require_logdir
    parse_period "${1:-}"
    set -- "$LOG_DIR"/domains/*.log
    [ -e "$1" ] || die 'Проверок доменов пока нет. Задайте DOMAINS_WATCH в /opt/etc/awg-monitor.conf.'
    cat "$@" | awk -v from="$from" -v to="$now" -v period="$period" '
        function kv(   i, p) {
            split("", f)
            for (i = 3; i <= NF; i++) { p = index($i, "="); if (p > 1) f[substr($i, 1, p - 1)] = substr($i, p + 1) }
        }
        function stages(d, side,   s, i, k, out) {
            split("dns tcp tls http cert ca iface err", s, " ")
            for (i = 1; i in s; i++) { k = d SUBSEP side SUBSEP s[i]; if (k in fail) out = out (out ? ", " : "") s[i] " " fail[k] }
            return out
        }
        function pct(ok, total) { return total ? sprintf("%.0f%%", 100 * ok / total) : "-" }
        {
            kv(); ts = f["ts"] + 0
            if (ts < from || ts > to) next
            d = f["domain"]; if (d == "") next
            if (!(d in n)) ord[++nd] = d
            n[d]++; w = f["wan"]; t = f["tun"]
            if (w != "nowan") { wn[d]++; if (w == "ok") wok[d]++; else fail[d, "w", w]++ }
            if (t == "ok") tok[d]++; else fail[d, "t", t]++
            last[d] = $1 " " substr($2, 1, 5) " напрямую=" w " туннель=" t
        }
        END {
            if (!nd) { print "За период " period " проверок доменов нет."; exit }
            print "Период " period ", доля успешных проверок:"
            for (i = 1; i <= nd; i++) {
                d = ord[i]
                printf "\n%s — проверок %d\n", d, n[d]
                ws = stages(d, "w"); ts = stages(d, "t")
                printf "  напрямую: %s%s\n", pct(wok[d], wn[d]), (ws ? " (сбои: " ws ")" : "")
                printf "  туннель:  %s%s\n", pct(tok[d], n[d]), (ts ? " (сбои: " ts ")" : "")
                printf "  последняя: %s\n", last[d]
            }
        }'
    if [ -f "$LOG_DIR/events.log" ]; then
        awk -v from="$from" '
            { t = $3; sub(/^ts=/, "", t); if (t + 0 < from) next }
            $4 == "domain_changed" { c++; l = l "\n  " $1 " " $2 " " $5 " " $6 " " $7 }
            END { if (c) printf "\nИзменения доступности: %d%s\n", c, l }' "$LOG_DIR/events.log" 2>/dev/null
    fi
}

cmd_snapshot() {
    require_logdir
    snapshot_write "${1:-manual}"
    echo "Снимок: $LOG_DIR/snapshots/$SNAP_NAME"
    echo "Хэш конфигурации: $SNAP_HASH"
}

cmd_snapshots() {
    require_logdir
    list=$(list_snapshots)
    if [ -n "$list" ]; then echo "$list"; else echo 'Снимков пока нет.'; fi
}

resolve_snapshot() {
    dir="$LOG_DIR/snapshots"
    if [ -f "$dir/$1" ]; then
        echo "$dir/$1"
    elif [ -f "$dir/$1.txt" ]; then
        echo "$dir/$1.txt"
    else
        m=$(list_snapshots | grep -F -- "$1" | tail -n 1 || :)
        [ -n "$m" ] || die "Снимок '$1' не найден. Список: awg-monitor snapshots"
        echo "$dir/$m"
    fi
}

cmd_diff() {
    require_logdir
    list=$(list_snapshots)
    case $# in
        0)
            [ "$(printf '%s\n' "$list" | grep -c .)" -ge 2 ] || die 'Нужно минимум два снимка.'
            a="$LOG_DIR/snapshots/$(printf '%s\n' "$list" | tail -n 2 | head -n 1)"
            b="$LOG_DIR/snapshots/$(printf '%s\n' "$list" | tail -n 1)" ;;
        1)
            a=$(resolve_snapshot "$1")
            b="$LOG_DIR/snapshots/$(printf '%s\n' "$list" | tail -n 1)" ;;
        2)
            a=$(resolve_snapshot "$1")
            b=$(resolve_snapshot "$2") ;;
        *) die 'diff принимает не больше двух снимков.' ;;
    esac
    WORK=$(mktemp -d /tmp/awg-monitor-diff.XXXXXX)
    trap 'rm -rf "$WORK"' 0
    for s in "$a" "$b"; do
        awk '/^## stable$/ { p = 1; next } /^## info$/ { p = 0 } p' "$s" > "$WORK/${s##*/}"
    done
    echo "Сравнение (только стабильная часть): ${a##*/} -> ${b##*/}"
    if diff -u "$WORK/${a##*/}" "$WORK/${b##*/}"; then
        echo 'Различий нет.'
    fi
}

cmd_bundle() {
    require_logdir
    days=${1:-3}
    is_num "$days" && [ "$days" -gt 0 ] || die 'Число дней — целое больше нуля.'
    now=$(date +%s)
    cutoff=$(date -d "@$((now - (days - 1) * 86400))" +%Y%m%d)
    name="awg-monitor-$(date +%Y-%m-%d_%H%M%S)"
    mkdir -p "$LOG_DIR/bundles"
    stage="bundles/.stage-$$"
    mkdir -p "$LOG_DIR/$stage"
    trap 'rm -rf "$LOG_DIR/$stage" "$LOG_DIR/bundles/$name.tar.gz.part"' 0
    {
        echo "# awg-monitor bundle $(date '+%Y-%m-%d %H:%M:%S %Z'), дней: $days"
        sec "config: $CONF"
        grep -v '^[[:space:]]*#' "$CONF" 2>/dev/null || :
        sec "report ${days}d"
        (cmd_report "${days}d") 2>&1 || :
    } > "$LOG_DIR/$stage/meta.txt"
    set -- "$stage/meta.txt"
    for f in "$LOG_DIR"/samples/*.log; do
        [ -e "$f" ] || continue
        d=${f##*/}
        d=$(echo "${d%.log}" | tr -d -)
        is_num "$d" && [ "$d" -ge "$cutoff" ] && set -- "$@" "samples/${f##*/}"
    done
    for f in "$LOG_DIR"/domains/*.log; do
        [ -e "$f" ] || continue
        d=${f##*/}
        d=$(echo "${d%.log}" | tr -d -)
        is_num "$d" && [ "$d" -ge "$cutoff" ] && set -- "$@" "domains/${f##*/}"
    done
    for f in events.log events.log.1; do
        [ -f "$LOG_DIR/$f" ] && set -- "$@" "$f"
    done
    [ -d "$LOG_DIR/snapshots" ] && set -- "$@" snapshots
    if [ -d "$LOG_DIR/dumps" ]; then
        for f in $(cd "$LOG_DIR" && find dumps -type f -mtime -"$days" 2>/dev/null); do
            set -- "$@" "$f"
        done
    fi
    (cd "$LOG_DIR" && tar -cf - "$@") | gzip > "$LOG_DIR/bundles/$name.tar.gz.part"
    # Refuse to keep the archive if a secret key slipped in.
    for key in $(awk '{ k = tolower($0); sub(/^[ \t]*/, "", k) }
            k ~ /^(privatekey|presharedkey)[ \t]*=/ { sub(/^[^=]*=[ \t]*/, ""); sub(/\r$/, ""); print }' "$AWG_CONF" 2>/dev/null); do
        if gzip -dc "$LOG_DIR/bundles/$name.tar.gz.part" | grep -Fq -- "$key"; then
            die 'В архив попал ключ из конфига AWG — архив удалён. Сообщите об ошибке.'
        fi
    done
    mv "$LOG_DIR/bundles/$name.tar.gz.part" "$LOG_DIR/bundles/$name.tar.gz"
    echo "Архив: $LOG_DIR/bundles/$name.tar.gz"
    echo 'Ключей в нём нет, но есть IP сервера и журнал роутера — передавайте только тем, кому доверяете.'
}

# ---------- install / uninstall ----------

owned() { [ ! -L "$1" ] && [ -f "$1" ] && grep -Fqx "$MARKER" "$1"; }

# Prints the filesystem type that holds path $1 (deepest existing ancestor).
fs_type() {
    p=$1
    while [ ! -d "$p" ]; do
        p=${p%/*}
        [ -n "$p" ] || p=/
    done
    awk -v p="$p" '{
        m = $2; gsub(/\\040/, " ", m)
        if ((m == "/" || p == m || index(p, m "/") == 1) && length(m) >= best) { best = length(m); t = $3 }
    } END { print t }' /proc/mounts
}

update_crontab() {
    # Preserve unrelated entries; remove only lines carrying our exact suffix.
    awk -v marker="$MARKER" '
        length($0) < length(marker) ||
        substr($0, length($0) - length(marker) + 1) != marker { print }
    ' "$CRONTAB" > "$WORK/crontab"
    if [ "$1" = add ]; then
        printf '* * * * * root %s collect >/dev/null 2>&1 %s\n' "$BIN" "$MARKER" >> "$WORK/crontab"
    fi
    if ! cmp -s "$CRONTAB" "$WORK/crontab"; then
        backup=$(mktemp "${CRONTAB}.awg-monitor-backup.XXXXXX")
        cp -p "$CRONTAB" "$backup"
        cat "$WORK/crontab" > "$CRONTAB"
        echo "Резервная копия расписания: $backup"
    fi
}

conf_domains() {
    cat <<EOF
# Домены (через пробел), которые раз в DOMAINS_EVERY минут проверяются напрямую
# и через туннель. Пусто — проверки выключены. Проверки идут параллельно.
DOMAINS_WATCH='$DOMAINS_WATCH'
DOMAINS_EVERY=$DOMAINS_EVERY
# Таймаут одной проверки домена, с.
DOMAIN_TIMEOUT=$DOMAIN_TIMEOUT
# Интерфейс провайдера для проверки «напрямую»; пусто — из маршрута по умолчанию.
WAN_IFACE=$WAN_IFACE
# DNS роутера для route-check.
DNS_SERVER=$DNS_SERVER
# Поиск доменов мимо туннеля (awg-monitor missed): 1 — включить. Нужен tcpdump
# (opkg install tcpdump); он слушает ответы DNS роутера клиентам на LAN_IFACE.
DNS_CAPTURE=$DNS_CAPTURE
LAN_IFACE=$LAN_IFACE
# Сколько новых доменов в минуту проверять напрямую и через туннель; повтор через столько часов.
MISSED_PROBES=$MISSED_PROBES
MISSED_RECHECK=$MISSED_RECHECK
# Считать подозрительными TLS-соединения, где сервер почти ничего не ответил (0 — выключить).
MISSED_STALL=$MISSED_STALL
EOF
}

cmd_install() {
    dir=
    while [ "$#" -gt 0 ]; do
        case $1 in
            --log-dir) [ "$#" -ge 2 ] || die '--log-dir требует путь.'; dir=$2; shift 2 ;;
            --log-dir=*) dir=${1#*=}; shift ;;
            *) usage; exit 1 ;;
        esac
    done
    [ "$(id -u)" = 0 ] || die 'Запустите под root в shell Entware.'
    self=$0
    [ -f "$self" ] && [ "$(sed -n 2p "$self")" = "$MARKER" ] ||
        die 'Запускайте как файл: sh awg-monitor.sh install'
    if [ -e "$BIN" ] || [ -L "$BIN" ]; then
        owned "$BIN" || die "$BIN уже существует и не принадлежит awg-monitor. Файл не изменён."
    fi
    if [ -e "$CONF" ] || [ -L "$CONF" ]; then
        owned "$CONF" || die "$CONF уже существует и не принадлежит awg-monitor. Файл не изменён."
    fi

    old_dir=
    if [ -f "$CONF" ]; then
        old_dir=$(load_conf; echo "$LOG_DIR")
    fi
    [ -n "$dir" ] || dir=${old_dir:-$DEFAULT_LOG_DIR}
    dir=${dir%/}
    case $dir in
        /?*) ;;
        *) die "Путь к логам должен быть абсолютным: '$dir'." ;;
    esac
    case $dir in
        *"'"*|*'
'*) die 'Путь к логам не должен содержать кавычки и переводы строк.' ;;
    esac
    fst=$(fs_type "$dir")
    case $fst in
        '') die "Не удалось определить файловую систему для $dir." ;;
        tmpfs|ramfs|rootfs|squashfs|jffs2|ubifs|proc|sysfs|devtmpfs)
            die "$dir находится в памяти роутера ($fst). Укажите каталог на внешнем диске, например /tmp/mnt/<диск>/awg-monitor." ;;
    esac

    if [ ! -x "$CRON_INIT" ]; then
        command -v opkg >/dev/null 2>&1 || die 'opkg не найден. Нужен shell Entware.'
        opkg update
        opkg install cron
    fi
    [ -x "$CRON_INIT" ] || die "После установки не найден $CRON_INIT."
    [ -f "$CRONTAB" ] || die "Не найден $CRONTAB. Проверьте установку cron."
    command -v awg >/dev/null 2>&1 || echo 'Предупреждение: awg не найден — handshake и Endpoint собираться не будут.'
    command -v curl >/dev/null 2>&1 || echo 'Предупреждение: curl не найден — HTTP-проверка отключена (opkg install curl).'
    [ -x "$SERVICE" ] || echo "Предупреждение: не найден $SERVICE — проверьте IFACE/SERVICE в $CONF."

    WORK=$(mktemp -d /tmp/awg-monitor-install.XXXXXX)
    trap 'rm -rf "$WORK"' 0

    mkdir -p "$dir"
    [ -f "$dir/$DIR_MARKER" ] || echo "$MARKER — каталог логов awg-monitor" > "$dir/$DIR_MARKER"

    if [ -f "$CONF" ]; then
        awk -v d="$dir" '/^LOG_DIR=/ { print "LOG_DIR='\''" d "'\''"; done = 1; next } { print }
            END { if (!done) print "LOG_DIR='\''" d "'\''" }' "$CONF" > "$WORK/conf"
    else
        cat > "$WORK/conf" <<EOF
$MARKER
# Настройки awg-monitor. Читаются при каждом запуске, перезапуск не нужен.
# Каталог логов (на внешнем диске). Сменить: sh awg-monitor.sh install --log-dir DIR
LOG_DIR='$dir'
# Интерфейс туннеля, интерфейс в NDM, init-скрипт и конфиг AWG.
IFACE=$IFACE
NDM_IFACE=$NDM_IFACE
SERVICE=$SERVICE
AWG_CONF=$AWG_CONF
# Адреса для ping через туннель и число пакетов.
PING_TARGETS='$PING_TARGETS'
PING_COUNT=$PING_COUNT
# HTTP-проверка через туннель раз в HTTP_EVERY минут (и всегда, если ping не прошёл).
# Пустой HTTP_URL — отключить; HTTP_EVERY=0 — только когда ping не прошёл.
HTTP_URL=$HTTP_URL
HTTP_EVERY=$HTTP_EVERY
# Handshake старше стольких секунд считается устаревшим.
HANDSHAKE_STALE=$HANDSHAKE_STALE
# RTT через туннель выше этого (мс) — состояние DEGRADED.
RTT_WARN=$RTT_WARN
# Проверять изменения конфигурации раз в столько минут.
CONFIG_EVERY=$CONFIG_EVERY
# Сколько дней хранить замеры, дампы и архивы.
KEEP_DAYS=$KEEP_DAYS
EOF
    fi
    # Configs from older versions get the new settings appended once.
    grep -q '^DOMAINS_WATCH=' "$WORK/conf" || conf_domains >> "$WORK/conf"
    mkdir -p "${CONF%/*}" "${BIN%/*}"
    cp "$WORK/conf" "$CONF.tmp.$$"
    mv "$CONF.tmp.$$" "$CONF"
    # Replace via rename so a running copy keeps reading the old inode.
    cp "$self" "$BIN.tmp.$$"
    chmod 755 "$BIN.tmp.$$"
    mv "$BIN.tmp.$$" "$BIN"

    update_crontab add
    "$CRON_INIT" restart

    echo "Установлено: $BIN, настройки $CONF"
    echo "Логи: $dir"
    if [ -n "$old_dir" ] && [ "$old_dir" != "$dir" ]; then
        echo "Прежний каталог $old_dir не тронут."
    fi
    echo 'Замер — каждую минуту. Первые данные: awg-monitor status (через минуту).'
    "$BIN" snapshot install || :
}

cmd_uninstall() {
    purge=0
    case $# in
        0) ;;
        1) [ "$1" = --purge ] || { usage; exit 1; }; purge=1 ;;
        *) usage; exit 1 ;;
    esac
    [ "$(id -u)" = 0 ] || die 'Запустите под root в shell Entware.'
    WORK=$(mktemp -d /tmp/awg-monitor-install.XXXXXX)
    trap 'rm -rf "$WORK"' 0
    load_conf
    if [ -f "$CRONTAB" ]; then
        update_crontab remove
        if [ -x "$CRON_INIT" ]; then "$CRON_INIT" restart; fi
    fi
    if owned "$BIN"; then rm -f "$BIN"; fi
    if owned "$CONF"; then rm -f "$CONF"; fi
    cap_stop
    rm -rf "$LOCK" "$NODIR_FLAG" "$DNS_MAP" "$CT_SEEN" "$CT_SEEN.tmp" "$INCLUDES" "$CAP_PID.missing"
    if logdir_ok; then
        if [ "$purge" = 1 ]; then
            rm -rf "$LOG_DIR/samples" "$LOG_DIR/domains" "$LOG_DIR/missed" "$LOG_DIR/dumps" "$LOG_DIR/snapshots" "$LOG_DIR/bundles"
            rm -f "$LOG_DIR/events.log" "$LOG_DIR/events.log.1" "$LOG_DIR/state" "$LOG_DIR/state.tmp" \
                "$LOG_DIR/domains.state" "$LOG_DIR/domains.state.tmp" "$LOG_DIR/missed.cache" "$LOG_DIR/$DIR_MARKER"
            rmdir "$LOG_DIR" 2>/dev/null || :
            echo "Логи удалены: $LOG_DIR"
        else
            echo "Логи сохранены: $LOG_DIR (удалить: uninstall --purge)"
        fi
    fi
    echo 'awg-monitor удалён. Пакет cron и другие задания сохранены.'
}

ACTION=${1:-}
[ "$#" -eq 0 ] || shift
case $ACTION in
    install) cmd_install "$@" ;;
    uninstall) cmd_uninstall "$@" ;;
    -h|--help|help|'') usage ;;
    collect|check|status|report|snapshot|snapshots|diff|bundle|check-domain|domains|route-check|missed)
        load_conf
        case $ACTION in
            collect) cmd_collect ;;
            check) cmd_check ;;
            status) cmd_status ;;
            report) [ "$#" -le 1 ] || die 'report принимает один аргумент.'; cmd_report "$@" ;;
            snapshot) cmd_snapshot "$@" ;;
            snapshots) cmd_snapshots ;;
            diff) cmd_diff "$@" ;;
            bundle) cmd_bundle "$@" ;;
            check-domain) cmd_check_domain "$@" ;;
            route-check) cmd_route_check "$@" ;;
            missed) [ "$#" -le 1 ] || die 'missed принимает один аргумент.'; cmd_missed "$@" ;;
            domains) [ "$#" -le 1 ] || die 'domains принимает один аргумент.'; cmd_domains "$@" ;;
        esac ;;
    *) usage; exit 1 ;;
esac
