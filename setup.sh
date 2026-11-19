#!/usr/bin/env bash
# Petal one-shot setup, from a fresh clone on a bare Debian/Ubuntu box.
#
#   ./setup.sh            install deps if needed, configure .env (interactive), build and start
#   ./setup.sh -y         same, but accept defaults / existing values without asking
#   ./setup.sh status     containers, pcap count, flow count
#   ./setup.sh logs       follow assembler + capture logs
#   ./setup.sh stop       stop all Petal containers
#   ./setup.sh reset      stop and DELETE the database (pcaps in traffic dir are kept)
#   ./clean.sh            remove Petal from Docker (separate script, see ./clean.sh --help)
#
# Any setting can be preset from the environment, e.g.
#   SERVICES="web:7001,pwn:7002" CAPTURE_IFACE=eth0 ./setup.sh -y
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")"

info() { printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- arguments
CMD="setup"
INTERACTIVE=1
for arg in "$@"; do
    case "$arg" in
        -y|--yes) INTERACTIVE=0 ;;
        -h|--help|help) sed -n '2,/^set /p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
        setup|status|logs|stop|reset) CMD="$arg" ;;
        *) die "unknown argument '$arg' (try --help)" ;;
    esac
done
[ -t 0 ] || INTERACTIVE=0

confirm() { # confirm "question"  (always yes in -y mode)
    [ "$INTERACTIVE" = 0 ] && return 0
    local a; read -r -p "$1 [Y/n] " a || true
    [[ ! "$a" =~ ^[Nn] ]]
}

# ---------------------------------------------------------------- root / sudo
# Docker and packet capture need root (or the docker group). Re-run ourselves with sudo if needed.
if [ "$(id -u)" != 0 ] && ! { command -v docker >/dev/null && docker info >/dev/null 2>&1; }; then
    command -v sudo >/dev/null || die "run this script as root (no sudo found)"
    info "root needed for docker, re-running with sudo..."
    exec sudo -E "$0" "$@"
fi

# Keep files we create owned by the person who ran the script, not root.
OWNER="${SUDO_USER:-$(id -un)}"
fix_owner() { [ "$(id -u)" = 0 ] && [ "$OWNER" != root ] && chown "$OWNER": "$@" 2>/dev/null || true; }

# ---------------------------------------------------------------- dependencies
apt_install() {
    command -v apt-get >/dev/null || die "missing '$*' and no apt-get: install it manually"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null
}

ensure_deps() {
    local missing=()
    command -v curl >/dev/null || missing+=(curl ca-certificates)
    command -v ip   >/dev/null || missing+=(iproute2)
    if [ ${#missing[@]} -gt 0 ]; then
        info "installing ${missing[*]}..."
        apt_install "${missing[@]}"
    fi

    if ! command -v docker >/dev/null; then
        confirm "Docker is not installed. Install it now (official get.docker.com script)?" \
            || die "docker is required: https://docs.docker.com/engine/install/"
        curl -fsSL https://get.docker.com | sh
        systemctl enable --now docker 2>/dev/null || true
        if [ "$OWNER" != root ]; then
            usermod -aG docker "$OWNER" 2>/dev/null && info "added $OWNER to the docker group (log out/in to use docker without sudo)"
        fi
        ok "docker installed"
    fi

    if ! docker info >/dev/null 2>&1; then
        systemctl start docker 2>/dev/null || true
        docker info >/dev/null 2>&1 || die "docker daemon is not running (try: systemctl status docker)"
    fi
}

compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose "$@"
    elif command -v docker-compose >/dev/null; then
        docker-compose "$@"
    else
        die "docker compose plugin not found (apt-get install docker-compose-plugin)"
    fi
}

# ---------------------------------------------------------------- .env helpers
get_env() { # value of KEY in .env, without quotes
    [ -f .env ] || return 0
    { grep -E "^$1=" .env || true; } | tail -n1 | cut -d= -f2- | sed -E "s/^[\"'](.*)[\"']$/\1/"
}

set_env() { # set KEY to VALUE in .env (single-quoted = literal, safe for regexes)
    KEY="$1" VAL="$2" awk '
        BEGIN { k = ENVIRON["KEY"]; line = k "='\''" ENVIRON["VAL"] "'\''" }
        $0 ~ "^" k "=" { if (!done) print line; done = 1; next }
        { print }
        END { if (!done) print line }
    ' .env > .env.tmp
    cat .env.tmp > .env && rm -f .env.tmp   # rewrite in place: keeps owner/permissions
}

ask() { # ask KEY "question" default [fresh]  -> env override > existing .env > default
    # With "fresh" the existing .env value is ignored (used for values derived from Docker,
    # which go stale when containers are recreated).
    local key="$1" question="$2" def="$3" cur answer
    if [ "${4:-}" = fresh ]; then cur="${!key:-}"; else cur="${!key:-$(get_env "$key")}"; fi
    [ -n "$cur" ] && def="$cur"
    if [ "$INTERACTIVE" = 1 ]; then
        read -r -p "$question [$def]: " answer || true
        answer="${answer:-$def}"
    else
        answer="$def"
    fi
    set_env "$key" "$answer"
    printf -v "$key" '%s' "$answer"
}

services_filter() { # "a:7001,b:10.0.0.1:7002" -> "tcp and (port 7001 or port 7002)"
    local ports
    ports="$(tr ',' '\n' <<<"$1" | awk -F: 'NF>=2 && $NF ~ /^ *[0-9]+ *$/ {gsub(/ /,"",$NF); print "port " $NF}' | sort -u | paste -sd'|' | sed 's/|/ or /g')"
    if [ -n "$ports" ]; then echo "tcp and ($ports)"; else echo "tcp and not port 22 and not port ${UI_PORT:-3000}"; fi
}

# Interactive picker: list running containers (except Petal's own) that publish TCP ports and let the
# user choose by number. Prints the chosen "name:hostport,..." on stdout (menu goes to stderr);
# prints nothing when there is nothing to pick or the user just presses Enter.
docker_pick() {
    command -v docker >/dev/null || return 0
    local rows
    rows="$(docker ps --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}|{{.Names}}|{{.Ports}}' 2>/dev/null \
        | awk -F'|' '$1 != "petal" && $1 != "tulip" {
            name = ($2 != "") ? $2 : $3
            n = split($4, p, ", ")
            for (i = 1; i <= n; i++)
                if (match(p[i], /:[0-9]+->[0-9]+\/tcp/)) {
                    m = substr(p[i], RSTART + 1, RLENGTH - 5); split(m, hp, "->")
                    key = $3 ":" hp[1]
                    if (!(key in seen)) { seen[key] = 1; print name "|" $3 "|" hp[1] "|" hp[2] "|" ($1 != "" ? $1 : "-") }
                }
          }' | sort -t'|' -k5,5 -k3,3n || true)"
    [ -n "$rows" ] || return 0

    {
        echo
        info "Running containers with published ports:"
        printf '      %-16s %-28s %-18s %s\n' NAME CONTAINER "HOST -> CONTAINER" PROJECT
        awk -F'|' '{ printf "  %2d) %-16s %-28s %-18s %s\n", NR, $1, $2, ":" $3 " -> " $4, $5 }' <<<"$rows"
    } >&2

    local pick
    read -r -p "Pick services (e.g. 1,3,4  2-5  a project name  'all'; Enter = type them yourself): " pick || true
    pick="$(tr -d '[:space:]' <<<"$pick")"
    [ -n "$pick" ] || return 0
    [ "$pick" = all ] && pick="1-$(wc -l <<<"$rows")"

    local nums=() part i
    IFS=, read -r -a parts <<<"$pick" || true
    for part in "${parts[@]}"; do
        if [[ "$part" =~ ^([0-9]+)-([0-9]+)$ ]]; then
            for ((i = BASH_REMATCH[1]; i <= BASH_REMATCH[2]; i++)); do nums+=("$i"); done
        elif [[ "$part" =~ ^[0-9]+$ ]]; then
            nums+=("$part")
        elif awk -F'|' -v p="$part" '$5 == p { f = 1 } END { exit !f }' <<<"$rows"; then
            mapfile -t -O "${#nums[@]}" nums < <(awk -F'|' -v p="$part" '$5 == p { print NR }' <<<"$rows")
        else
            warn "ignoring '$part' (not a number, range or project name)" >&2
        fi
    done
    printf '%s\n' "${nums[@]}" | awk -F'|' 'NR == FNR { want[$1] = 1; next } (FNR in want) { print $1 ":" $3 }' - <(cat <<<"$rows") \
        | paste -sd, - || true
}

vpn_ranges() { # subnets of VPN-style interfaces (tun*, wg*, tap*, ppp*), comma separated
    ip -4 -o addr show 2>/dev/null | awk '$2 ~ /^(tun|wg|tap|ppp)/ { print $4 }' | paste -sd, - || true
}

host_ips() { # "iface ip" for this box's IPv4 addresses, without loopback and Docker bridges
    ip -4 -o addr show 2>/dev/null | awk '$2 !~ /^(lo|docker[0-9]*|br-|veth)/ { split($4, a, "/"); print $2, a[1] }'
}

bind_ok() { # true when $1 is 0.0.0.0 or assigned to an interface of this box
    [ "$1" = 0.0.0.0 ] && return 0
    ip -4 -o addr show 2>/dev/null | awk -v ip="$1" '{ split($4, a, "/") } a[1] == ip { f = 1 } END { exit !f }'
}

port_busy() { # true when something other than Petal's own frontend listens on TCP port $1
    ss -ltnH "sport = :$1" 2>/dev/null | grep -q . || return 1
    # Re-running setup: our own frontend holding the port is fine, compose will recreate it.
    [ -z "$(docker ps -q --filter "publish=$1" --filter "label=com.docker.compose.project=petal" \
                                --filter "label=com.docker.compose.service=frontend" 2>/dev/null)" ]
}

free_port() { # first port from $1 upwards (max +100) that is not busy
    local p
    for ((p = $1; p <= $1 + 100; p++)); do port_busy "$p" || { echo "$p"; return 0; }; done
    echo "$1"
}

normalize_services() { # strip spaces, empty entries and stray ':'  ("web:7001: , pwn:1337" -> "web:7001,pwn:1337")
    { tr ',' '\n' <<<"$1" | sed -E 's/[[:space:]]+//g; s/:+$//; s/^:+//' | grep -v '^$' | paste -sd, -; } || true
}

# CTF services running as Docker containers on this host: traffic to them never crosses the host's
# public port on eth0 when it arrives through a tunnel/reverse proxy container, and the UI must match
# the container ip:port that shows up in the capture. For every "name:hostport" entry published by a
# running container this resolves the internal port and EVERY network the container is attached to
# (ip + bridge interface): the capture listens on all of those bridges. Entries that are not Docker
# containers are captured on the host interface ($def_iface). In the UI each service gets one ip: the
# one on the network it shares with the most other selected services (where proxy traffic flows).
# Sets: SERVICES_UI, DOCKER_IFACE (comma separated), DOCKER_FILTER (both empty when nothing is in Docker),
#       TRUSTED_PROXIES (container IPs on those networks, comma separated).
docker_bridge() { # bridge interface of Docker network id $1
    local b
    b="$(docker network inspect -f '{{index .Options "com.docker.network.bridge.name"}}' "$1" 2>/dev/null || true)"
    [[ -z "$b" || "$b" == "<no value>" ]] && b="br-${1:0:12}"
    echo "$b"
}

docker_resolve() {
    SERVICES_UI="$1" DOCKER_IFACE="" DOCKER_FILTER="" TRUSTED_PROXIES=""
    command -v docker >/dev/null || return 0
    local entry name port cid cname cport nets idx=0 rows="" hostsvc="" entries=() parts=()
    IFS=, read -r -a entries <<<"$1" || true
    for entry in "${entries[@]}"; do
        idx=$((idx + 1))
        IFS=: read -r -a parts <<<"$entry" || true
        if [ "${#parts[@]}" = 3 ] && [[ "${parts[2]}" =~ ^[0-9]+$ ]]; then      # name:ip:port, not Docker
            hostsvc+="$idx|${parts[0]}|${parts[1]}|${parts[2]}"$'\n'; continue
        fi
        [ "${#parts[@]}" = 2 ] && [[ "${parts[1]}" =~ ^[0-9]+$ ]] || continue
        name="${parts[0]}" port="${parts[1]}"
        cid="$(docker ps -q --filter "publish=$port" 2>/dev/null | head -n1 || true)"
        cport=""
        [ -n "$cid" ] && cport="$(docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{$p}} {{.HostPort}}{{"\n"}}{{end}}{{end}}' "$cid" 2>/dev/null \
            | awk -v hp="$port" '$2 == hp && $1 ~ /\/tcp$/ { split($1, a, "/"); print a[1]; exit }' || true)"
        nets=""
        [ -n "$cport" ] && nets="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.NetworkID}}{{"\n"}}{{end}}' "$cid" 2>/dev/null | awk 'NF == 2' || true)"
        if [ -z "$nets" ]; then                                                   # not a container: host port
            hostsvc+="$idx|$name|${VM_IP:-}|$port"$'\n'; continue
        fi
        cname="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##' || true)"
        while read -r ip netid; do
            rows+="$idx|$name|$cport|$ip|$netid|$(docker_bridge "$netid")|$cname|$port"$'\n'
        done <<<"$nets"
    done
    [ -n "$rows" ] || return 0

    # Every container on those networks (nginx, tunnel, ...) may proxy requests: trust them when
    # reading the real client IP from X-Forwarded-For / X-Real-IP (assembler tags flows src:<ip>).
    local netid_list
    netid_list="$(cut -d'|' -f5 <<<"$rows" | sort -u)"
    TRUSTED_PROXIES="$(for n in $netid_list; do
            docker network inspect -f '{{range .Containers}}{{.IPv4Address}} {{end}}' "$n" 2>/dev/null || true
        done | tr ' ' '\n' | sed 's#/.*##' | grep -v '^$' | sort -uV | paste -sd, - || true)"

    # idx|name|cport|ip|netid|bridge|container|hostport  ->  UI entries, filter, interface list
    local out
    out="$(printf '%s%s' "$rows" "$hostsvc" | awk -F'|' -v hif="${def_iface:-}" '
        NF == 8 { n++; r[n] = $0; if (!(($1 SUBSEP $5) in seen)) { seen[$1 SUBSEP $5] = 1; cnt[$5]++ } next }
        NF == 4 { h[$1] = $0; next }
        END {
            for (i = 1; i <= n; i++) {
                split(r[i], f, "|")
                printf "INFO|  docker: %s  host :%s -> %s %s:%s on %s\n", f[2], f[8], f[7], f[4], f[3], f[6]
                if (!(f[1] in best) || cnt[f[5]] > bestc[f[1]]) { best[f[1]] = f[2] ":" f[4] ":" f[3]; bestc[f[1]] = cnt[f[5]] }
                hosts[f[1]] = hosts[f[1]] (hosts[f[1]] ? " or " : "") "host " f[4]; port[f[1]] = f[3]
                if (!(f[6] in ifs)) { ifs[f[6]] = 1; iflist = iflist (iflist ? "," : "") f[6] }
            }
            for (k in h) {
                split(h[k], f, "|"); best[k] = f[2] ":" f[3] ":" f[4]; hosts[k] = "host " f[3]; port[k] = f[4]
                printf "INFO|  host:   %s  %s:%s on %s\n", f[2], f[3], f[4], hif
                if (hif != "" && !(hif in ifs)) { ifs[hif] = 1; iflist = iflist (iflist ? "," : "") hif }
            }
            for (k in best) { ui[k + 0] = best[k]; flt = flt (flt ? " or " : "") "(port " port[k] " and (" hosts[k] "))" }
            for (k = 1; k <= 9999; k++) if (k in ui) uis = uis (uis ? "," : "") ui[k]
            print "UI|" uis; print "IF|" iflist; print "FILTER|tcp and (" flt ")"
        }')"
    while IFS='|' read -r tag val; do
        case "$tag" in
            INFO)   info "$val" ;;
            UI)     SERVICES_UI="$val" ;;
            IF)     DOCKER_IFACE="$val" ;;
            FILTER) DOCKER_FILTER="$val" ;;
        esac
    done <<<"$out"
    [[ "$DOCKER_IFACE" == *,* ]] && info "  services span several networks: capturing on all of them ($DOCKER_IFACE)"
    return 0
}

# ---------------------------------------------------------------- commands
cmd_status() {
    compose ps
    local dir; dir="$(get_env TRAFFIC_DIR_HOST)"; dir="${dir:-./traffic}"
    echo
    info "pcaps ready:   $(find "$dir" -maxdepth 1 -name '*.pcap*' 2>/dev/null | wc -l)  ($dir)"
    info "pcaps writing: $(find "$dir/.incoming" -maxdepth 1 -name '*.pcap*' 2>/dev/null | wc -l)"
    info "flows in db:   $(compose exec -T timescale psql -U petal -d petal -tAc 'SELECT count(*) FROM flow' 2>/dev/null || echo '?')"
}

cmd_setup() {
    ensure_deps

    if [ ! -f .env ]; then
        cp .env.example .env
        fix_owner .env
        # Clear the example's placeholder values so the detected defaults below are offered instead
        for k in TRAFFIC_DIR_HOST TICK_START VM_IP SERVICES CAPTURE_IFACE CAPTURE_FILTER; do set_env "$k" ""; done
        ok "created .env from .env.example"
    fi
    # .env carried over from a Tulip install: the database is now called petal
    if get_env TIMESCALE | grep -q 'tulip'; then
        set_env TIMESCALE "postgres://petal@timescale:5432/petal"
        warn "migrated .env from Tulip: TIMESCALE now points to the petal database (a fresh one; old pcaps are re-imported)"
    fi

    local def_iface def_ip
    def_iface="$(ip route show default 2>/dev/null | awk '{print $5; exit}')"
    def_ip="$(ip -4 -o addr show "${def_iface:-lo}" 2>/dev/null | awk '{split($4,a,"/"); print a[1]; exit}')"

    echo
    info "Configuration (Enter keeps the value in brackets)"
    ask VM_IP           "Your vulnbox IP"                                         "$def_ip"
    local def_port; def_port="$(free_port 3000)"
    [ "$def_port" != 3000 ] && warn "port 3000 is already in use: $(ss -ltnpH 'sport = :3000' 2>/dev/null | grep -o 'users:(("[^"]*' | head -n1 | cut -d'"' -f2)"
    if [ -n "$(get_env UI_PORT)" ] && port_busy "$(get_env UI_PORT)"; then set_env UI_PORT ""; fi   # saved port got taken
    ask UI_PORT         "Web UI port"                                             "$def_port"
    # Which IP the dashboard listens on, e.g. the competition network IP so it is reachable there only.
    local ips; ips="$(host_ips)"
    if [ "$INTERACTIVE" = 1 ] && [ -n "$ips" ]; then
        echo; info "IP addresses on this box (0.0.0.0 = all of them):"
        awk '{ printf "      %-16s %s\n", $2, $1 }' <<<"$ips"
    fi
    if [ -n "$(get_env UI_BIND)" ] && ! bind_ok "$(get_env UI_BIND)"; then
        warn "saved dashboard IP $(get_env UI_BIND) is no longer on this box"; set_env UI_BIND ""
    fi
    ask UI_BIND         "Dashboard IP (listen address; 0.0.0.0 = all)"            "0.0.0.0"

    if [ "$INTERACTIVE" = 1 ] && [ -z "${SERVICES:-}" ]; then   # not preset from the environment
        local picked; picked="$(docker_pick)"
        [ -n "$picked" ] && SERVICES="$picked"   # shown below as the default, Enter accepts it
    fi
    ask SERVICES        "Services as name:port,name:port (e.g. web:8080,pwn:1337)" ""
    SERVICES="$(normalize_services "$SERVICES")"
    set_env SERVICES "$SERVICES"

    docker_resolve "$SERVICES"
    set_env SERVICES_UI "$SERVICES_UI"
    set_env TRUSTED_PROXIES "$TRUSTED_PROXIES"
    [ -n "$TRUSTED_PROXIES" ] && info "attacker IP tags (src:<ip>): seeing through $(tr ',' '\n' <<<"$TRUSTED_PROXIES" | wc -l) proxy container(s) on those networks"
    if [ -n "$DOCKER_IFACE" ]; then
        ok "services run in Docker: capturing on the container bridge, not the host interface"
        ask CAPTURE_IFACE  "Interface(s) to capture on, comma separated ('none' = off)" "$DOCKER_IFACE"  fresh
        ask CAPTURE_FILTER "BPF capture filter"                                   "$DOCKER_FILTER" fresh
    else
        ask CAPTURE_IFACE  "Interface(s) to capture on, comma separated ('none' = off)" "$def_iface"
        ask CAPTURE_FILTER "BPF capture filter"                                   "$(services_filter "$SERVICES")"
    fi
    # Attacker IP tags (src:<ip>) only for the competition VPN, not Docker gateways / LAN PCs.
    local def_vpn; def_vpn="$(vpn_ranges)"
    [ -n "$def_vpn" ] && info "VPN-like interfaces found: $def_vpn"
    ask VPN_RANGES      "Competition VPN range(s) for attacker tags (CIDR, comma separated; 'none' = tag all)" "$def_vpn"
    VPN_RANGES="$(tr -d '[:space:]' <<<"$VPN_RANGES")"; [ "$VPN_RANGES" = none ] && VPN_RANGES=""
    set_env VPN_RANGES "$VPN_RANGES"
    ask CAPTURE_ROTATE  "Seconds per pcap file"                                   "60"
    ask TICK_START      "CTF start time (ISO 8601, e.g. 2026-11-17T10:30:00+08:00)" "$(date -Iseconds)"
    ask TICK_LENGTH     "Tick length in ms"                                       "120000"
    ask FLAG_REGEX      "Flag regex"                                              "[A-Z0-9]{31}="
    ask TRAFFIC_DIR_HOST "Folder for pcaps on this host"                          "./traffic"

    # ---- validation
    if [ "$CAPTURE_IFACE" = "none" ]; then
        set_env CAPTURE_IFACE ""
        CAPTURE_IFACE=""
        [ -n "$(get_env PCAP_OVER_IP)" ] || warn "capture disabled and PCAP_OVER_IP empty: only pcaps you copy into $TRAFFIC_DIR_HOST will be imported"
    else
        CAPTURE_IFACE="$(tr -d '[:space:]' <<<"$CAPTURE_IFACE")"; set_env CAPTURE_IFACE "$CAPTURE_IFACE"
        local i ifs=()
        IFS=, read -r -a ifs <<<"$CAPTURE_IFACE" || true
        for i in "${ifs[@]}"; do
            ip link show "$i" >/dev/null 2>&1 || die "interface '$i' does not exist (see: ip -br link)"
        done
    fi
    [[ "$VM_IP" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "VM_IP '$VM_IP' is not an IPv4 address (re-run ./setup.sh)"
    local r rs=()
    IFS=, read -r -a rs <<<"$VPN_RANGES" || true
    for r in "${rs[@]}"; do
        [[ "$r" =~ ^[0-9A-Fa-f:.]+(/[0-9]{1,3})?$ ]] || die "VPN range '$r' is not an IP or CIDR (e.g. 10.60.0.0/16)"
    done
    [[ "$UI_BIND" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "UI_BIND '$UI_BIND' is not an IPv4 address (re-run ./setup.sh)"
    bind_ok "$UI_BIND" || die "dashboard IP $UI_BIND is not on this box (ip -br addr). Bring that interface/VPN up first, or add it: ip addr add $UI_BIND/32 dev ${def_iface:-eth0}"
    [[ "$UI_PORT" =~ ^[0-9]+$ ]] && [ "$UI_PORT" -ge 1 ] && [ "$UI_PORT" -le 65535 ] || die "UI_PORT '$UI_PORT' is not a valid port"
    ! port_busy "$UI_PORT" || die "port $UI_PORT is already in use (free port: $(free_port "$UI_PORT")); re-run ./setup.sh"
    date -d "$TICK_START" >/dev/null 2>&1 || die "TICK_START '$TICK_START' is not a valid date"
    [[ "$TICK_LENGTH" =~ ^[0-9]+$ ]]    || die "TICK_LENGTH must be a number of milliseconds"
    [[ "$CAPTURE_ROTATE" =~ ^[0-9]+$ ]] || die "CAPTURE_ROTATE must be a number of seconds"
    [ -n "$SERVICES" ] || warn "no SERVICES set: everything will show up under 'other' in the UI"
    if [ "$(date -d "$TICK_START" +%s)" -gt "$(date +%s)" ]; then
        warn "TICK_START is in the future: tick numbers will look odd until the game starts"
    fi

    mkdir -p "$TRAFFIC_DIR_HOST"
    fix_owner "$TRAFFIC_DIR_HOST"

    echo
    info "Building and starting containers (first build takes a few minutes)..."
    compose up -d --build --remove-orphans

    local UI_HOST="$UI_BIND"; [ "$UI_BIND" = 0.0.0.0 ] && UI_HOST=127.0.0.1
    info "Waiting for the web UI..."
    for _ in $(seq 1 60); do
        curl -fs -o /dev/null http://${UI_HOST}:${UI_PORT}/ && break
        sleep 2
    done
    curl -fs -o /dev/null http://${UI_HOST}:${UI_PORT}/ || warn "frontend not answering yet, check: ./setup.sh logs"

    echo
    cmd_status
    echo
    if [ "$UI_BIND" = 0.0.0.0 ]; then
        ok "Petal is up:  http://${VM_IP:-localhost}:${UI_PORT}   (listening on all IPs: $(host_ips | awk '{print $2}' | paste -sd' ' -))"
    else
        ok "Petal is up:  http://${UI_BIND}:${UI_PORT}   (dashboard only on this IP)"
    fi
    [ -n "$CAPTURE_IFACE" ] && ok "Capturing on $CAPTURE_IFACE ($CAPTURE_FILTER), flows appear every ~${CAPTURE_ROTATE}s once traffic arrives"
    if [ -n "$VPN_RANGES" ]; then ok "Attacker tags src:<ip> only for VPN range(s): $VPN_RANGES"
    else info "Attacker tags src:<ip> for every client IP (set a VPN range to limit them)"; fi
    [ -n "$DOCKER_IFACE" ] && warn "Docker container IPs can change: re-run ./setup.sh after recreating the CTF containers"
    info "Change settings: re-run ./setup.sh   |   progress: ./setup.sh status"
}


case "$CMD" in
    setup)  cmd_setup ;;
    status) cmd_status ;;
    logs)   compose logs -f --tail 50 assembler capture ;;
    stop)   compose down ;;
    reset)
        read -r -p "Delete the Petal database volume? Pcaps are kept. [y/N] " a
        [[ "$a" =~ ^[Yy]$ ]] || exit 0
        compose down -v
        ok "database wiped; run ./setup.sh to start fresh (existing pcaps will be re-imported)"
        ;;
esac
