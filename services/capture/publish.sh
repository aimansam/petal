#!/bin/sh
# Called by tcpdump (-z) when a capture file is complete.
# Moving it out of the staging dir is atomic, so the assembler never sees a half-written pcap.
mv "$1" "${OUT_DIR:-/traffic}/"
