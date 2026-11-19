#!/bin/sh
# Rotating tcpdump capture for Petal.
#   CAPTURE_IFACE   interface(s) to sniff, comma separated (empty = capture disabled)
#   CAPTURE_FILTER  BPF filter expression (applied on every interface)
#   CAPTURE_ROTATE  seconds per pcap file
# One tcpdump runs per interface (Docker bridges are separate L2 segments, so a packet is only seen
# on one of them: no duplicates). Files are named <iface>_<time>.pcap.
set -eu

IFACES="$(echo "${CAPTURE_IFACE:-}" | tr -d ' ')"
FILTER="${CAPTURE_FILTER:-tcp}"
ROTATE="${CAPTURE_ROTATE:-60}"
OUT_DIR="${OUT_DIR:-/traffic}"
STAGE="$OUT_DIR/.incoming"
export OUT_DIR

if [ -z "$IFACES" ]; then
    echo "[capture] CAPTURE_IFACE is empty, capture disabled."
    exec sleep infinity
fi

mkdir -p "$STAGE"

# Hand staged pcaps matching glob $1 that hold packets to the assembler (header-only files are dropped).
# Files left by a stopped tcpdump are valid up to their last packet.
publish_staged() {
    for f in $1; do
        [ -e "$f" ] || continue
        if [ "$(stat -c %s "$f")" -gt 24 ]; then mv "$f" "$OUT_DIR/"; else rm -f "$f"; fi
    done
}

# Capture loop for one interface. tcpdump -G only rotates when a packet arrives after the interval,
# so the last file of a burst would sit in $STAGE until the next packet. The watchdog restarts tcpdump
# once the current file holds packets and has been idle for $ROTATE seconds; the link is quiet then,
# so nothing is lost.
capture() {
    iface="$1" glob="$STAGE/$1_*.pcap" pid=""
    trap '[ -n "$pid" ] && kill "$pid" 2>/dev/null; wait 2>/dev/null; publish_staged "$glob"; exit 0' TERM INT
    while true; do
        publish_staged "$glob"
        # shellcheck disable=SC2086  # the filter is intentionally word-split into tcpdump args
        tcpdump -i "$iface" -n -s 0 -U -Z root \
            -G "$ROTATE" -w "$STAGE/${iface}_%Y-%m-%d_%H-%M-%S.pcap" \
            -z /usr/local/bin/publish.sh \
            $FILTER &
        pid=$!
        idle=0
        while kill -0 "$pid" 2>/dev/null; do
            sleep 5
            cur="$(ls -t $glob 2>/dev/null | head -n1 || true)"
            [ -n "$cur" ] || continue
            size="$(stat -c %s "$cur" 2>/dev/null)" || continue   # may have just been rotated away
            mtime="$(stat -c %Y "$cur" 2>/dev/null)" || continue
            [ "$size" -gt 24 ] || continue
            if [ $(( $(date +%s) - mtime )) -ge "$ROTATE" ]; then
                idle=1
                kill "$pid" 2>/dev/null || true
                break
            fi
        done
        wait "$pid" 2>/dev/null || true
        pid=""
        if [ "$idle" = 0 ]; then
            publish_staged "$glob"
            echo "[capture:$iface] tcpdump exited" >&2
            exit 1
        fi
    done
}

publish_staged "$STAGE/*.pcap"   # leftovers from a previous run, whatever interface they came from
echo "[capture] ifaces=$IFACES rotate=${ROTATE}s filter='$FILTER' -> $OUT_DIR"
PIDS=""
for i in $(echo "$IFACES" | tr ',' ' '); do
    capture "$i" &
    PIDS="$PIDS $!"
done
# shellcheck disable=SC2086
trap 'kill $PIDS 2>/dev/null; wait; exit 0' TERM INT

# If any interface loop dies (bad interface, tcpdump crash), stop everything so Docker restarts us.
while true; do
    for p in $PIDS; do
        kill -0 "$p" 2>/dev/null || { echo "[capture] a capture loop exited, restarting container" >&2; kill $PIDS 2>/dev/null; wait; exit 1; }
    done
    sleep 5
done
