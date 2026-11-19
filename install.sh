#!/usr/bin/env bash
# Petal installer for a fresh pwn box: Attack/Defense CTF traffic analyzer (capture container,
# setup.sh, Docker-hosted services, attacker src:<ip> tags).
#
#   bash install.sh            clone Petal into ./petal (or update it), then run ./setup.sh
#   bash install.sh -y         same, non-interactive (all setup.sh options are passed through)
#   curl -fsSL https://raw.githubusercontent.com/aimansam/petal/main/install.sh | bash
#
#   PETAL_DIR=/opt/petal bash install.sh    install somewhere else
#   PETAL_REF=<branch|tag>  bash install.sh  install another branch/tag (default: main)
#
# Running it again on an existing install pulls the latest changes and re-runs ./setup.sh.
set -euo pipefail

REPO="${PETAL_REPO:-https://github.com/aimansam/petal.git}"
REF="${PETAL_REF:-main}"
PETAL_DIR="${PETAL_DIR:-$PWD/petal}"

info() { printf '\033[1;34m[*]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

SUDO=""
if [ "$(id -u)" != 0 ]; then
    command -v sudo >/dev/null || die "run as root (no sudo found)"
    SUDO="sudo"
fi

if ! command -v git >/dev/null; then
    command -v apt-get >/dev/null || die "git is missing and no apt-get: install git manually"
    info "installing git..."
    $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq
    $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git ca-certificates >/dev/null
fi

if [ -f "$PETAL_DIR/setup.sh" ] && [ -d "$PETAL_DIR/.git" ]; then
    info "Petal already installed in $PETAL_DIR, updating"
    if [ -n "$(git -C "$PETAL_DIR" status --porcelain --untracked-files=no)" ]; then
        warn "local changes in $PETAL_DIR, not updating (commit or stash them to get updates)"
    elif ! git -C "$PETAL_DIR" pull -q --ff-only 2>/dev/null; then
        warn "could not fast-forward $PETAL_DIR (offline, or it is not on a tracking branch); using it as is"
    fi
elif [ -e "$PETAL_DIR" ] && [ -n "$(ls -A "$PETAL_DIR" 2>/dev/null)" ]; then
    die "$PETAL_DIR exists and is not a Petal install (set PETAL_DIR=...)"
else
    info "cloning $REPO ($REF) into $PETAL_DIR"
    git clone -q --branch "$REF" "$REPO" "$PETAL_DIR"
    ok "Petal cloned"
fi

cd "$PETAL_DIR"
# When piped (curl | bash) stdin is the script itself; give setup.sh the terminal for its questions.
if [ ! -t 0 ] && { : </dev/tty; } 2>/dev/null; then
    exec ./setup.sh "$@" </dev/tty
fi
exec ./setup.sh "$@"
