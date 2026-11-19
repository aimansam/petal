#!/usr/bin/env bash
# Remove Petal from Docker: containers, network, database volume and petal-* images, plus leftovers
# of an old Tulip install (tulip-* images, tulip_* volumes/networks). Keeps .env and the pcaps, so
# ./install.sh can rebuild and re-import later.
#
#   ./clean.sh            show what will be removed, ask, then remove it
#   ./clean.sh --pcaps    also delete the captured pcaps in the traffic folder
#   ./clean.sh -y         no questions (pcaps are kept unless --pcaps is given)
#
# To remove Petal completely, run this and then delete the folder.
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")"

info() { printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

INTERACTIVE=1
CLEAN_PCAPS=0
for arg in "$@"; do
    case "$arg" in
        -y|--yes) INTERACTIVE=0 ;;
        --pcaps) CLEAN_PCAPS=1 ;;
        -h|--help|help) sed -n '2,/^set /p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
        *) die "unknown argument '$arg' (try --help)" ;;
    esac
done
[ -t 0 ] || INTERACTIVE=0

# Docker needs root (or the docker group). Re-run ourselves with sudo if needed.
if [ "$(id -u)" != 0 ] && command -v docker >/dev/null && ! docker info >/dev/null 2>&1; then
    command -v sudo >/dev/null || die "run this script as root (no sudo found)"
    info "root needed for docker, re-running with sudo..."
    exec sudo -E "$0" "$@"
fi

compose() {
    if docker compose version >/dev/null 2>&1; then
        docker compose "$@"
    elif command -v docker-compose >/dev/null; then
        docker-compose "$@"
    else
        return 1
    fi
}

get_env() { # value of KEY in .env, without quotes
    [ -f .env ] || return 0
    { grep -E "^$1=" .env || true; } | tail -n1 | cut -d= -f2- | sed -E "s/^[\"'](.*)[\"']$/\1/"
}

# Remove everything Petal created in Docker (and leftovers of an old Tulip install). Keeps .env and,
# unless asked, the pcaps, so ./install.sh can rebuild and re-import later.
cmd_clean() {
    command -v docker >/dev/null || die "docker is not installed: nothing to clean"
    local dir pcaps containers images volumes networks
    dir="$(get_env TRAFFIC_DIR_HOST)"; dir="${dir:-./traffic}"
    pcaps="$(find "$dir" -name '*.pcap*' 2>/dev/null | wc -l)"
    containers="$(for p in petal tulip; do docker ps -a --format '{{.Names}}' --filter "label=com.docker.compose.project=$p" 2>/dev/null; done | sort | paste -sd' ' -)"
    images="$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -E '^(petal|tulip)-' | sort | paste -sd' ' - || true)"
    volumes="$(docker volume ls -q 2>/dev/null | grep -E '^(petal|tulip)_' | sort | paste -sd' ' - || true)"
    networks="$(docker network ls --format '{{.Name}}' 2>/dev/null | grep -E '^(petal|tulip)_' | sort | paste -sd' ' - || true)"

    if [ -z "$containers$images$volumes$networks" ] && { [ "$CLEAN_PCAPS" = 0 ] || [ "$pcaps" = 0 ]; }; then
        ok "nothing to clean"; return 0
    fi
    info "This removes:"
    echo "    containers: ${containers:-none}"
    echo "    images:     ${images:-none}"
    echo "    volumes:    ${volumes:-none}   (the database: all imported flows)"
    echo "    networks:   ${networks:-none}"
    if [ "$CLEAN_PCAPS" = 1 ]; then echo "    pcaps:      $pcaps in $dir"
    else echo "    kept:       .env and $pcaps pcaps in $dir (./install.sh rebuilds and re-imports them)"; fi
    if [ "$INTERACTIVE" = 1 ]; then
        local a; read -r -p "Continue? [y/N] " a || true
        [[ "$a" =~ ^[Yy]$ ]] || { info "cancelled"; return 0; }
        if [ "$CLEAN_PCAPS" = 0 ] && [ "$pcaps" -gt 0 ]; then
            read -r -p "Also delete the $pcaps captured pcaps? [y/N] " a || true
            [[ "$a" =~ ^[Yy]$ ]] && CLEAN_PCAPS=1
        fi
    fi

    [ -f docker-compose.yml ] && compose down -v --remove-orphans >/dev/null 2>&1 || true
    # shellcheck disable=SC2086  # word-split lists of names
    {
        [ -n "$containers" ] && docker rm -f $containers >/dev/null 2>&1
        [ -n "$images" ]     && docker image rm -f $images >/dev/null 2>&1
        [ -n "$volumes" ]    && docker volume rm -f $volumes >/dev/null 2>&1
        [ -n "$networks" ]   && docker network rm $networks >/dev/null 2>&1
    } || true
    if [ "$CLEAN_PCAPS" = 1 ]; then
        find "$dir" -name '*.pcap*' -type f -delete 2>/dev/null || true
        ok "deleted $pcaps pcaps"
    fi

    local left
    left="$(docker ps -a --format '{{.Names}}' --filter label=com.docker.compose.project=petal 2>/dev/null; docker images --format '{{.Repository}}' 2>/dev/null | grep -E '^(petal|tulip)-'; docker volume ls -q 2>/dev/null | grep -E '^(petal|tulip)_')" || true
    if [ -n "$left" ]; then warn "could not remove: $(paste -sd' ' - <<<"$left")"
    else ok "Petal removed from Docker. Rebuild any time with ./install.sh; delete this folder to remove it completely"; fi
}

cmd_clean
