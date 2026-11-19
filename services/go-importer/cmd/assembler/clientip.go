package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Real client (attacker) IP behind reverse proxies.
//
// When requests reach a service through a proxy (nginx, a tunnel container...), the flow's source IP is
// the proxy. The proxy chain records the original client in X-Forwarded-For / X-Real-IP /
// CF-Connecting-IP. Clients can forge those headers, so the chain is walked right-to-left and only
// *trusted* hops (the proxies themselves) are skipped: the first untrusted hop is the client.
var trustedProxiesRaw = flag.String("trusted-proxies", "",
	"Comma separated IPs/CIDRs of reverse proxies (e.g. nginx containers). Used to find the real client IP "+
		"in X-Forwarded-For/X-Real-IP, which is added to HTTP flows as tag 'src:<ip>'")

var trustedProxies []netip.Prefix

var vpnRangesRaw = flag.String("vpn-ranges", "",
	"Comma separated CIDRs of the competition VPN. When set, src:<ip> tags are only added for client IPs inside them")

var vpnRanges []netip.Prefix

// wantSrcTag reports whether a real client IP should become a src:<ip> tag: always when no VPN range
// is configured, otherwise only for addresses inside it (skips Docker gateways, LAN PCs, ...).
func wantSrcTag(ip netip.Addr, ranges []netip.Prefix) bool {
	return len(ranges) == 0 || isTrusted(ip.Unmap(), ranges)
}

func ParseTrustedProxies(raw string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			log.Printf("WARNING: ignoring invalid trusted proxy %q", part)
		}
	}
	return out
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseHop parses one header value element: "1.2.3.4", "1.2.3.4:5678", "[::1]:80", "::1".
func parseHop(s string) (netip.Addr, bool) {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// realClientIP returns the original client of an HTTP request that `peer` (the flow's source) sent us.
// Headers are only believed when peer is a trusted proxy: a client talking to us directly can write
// anything in them, and is itself the source. Returns false when there is nothing better than peer.
func realClientIP(h http.Header, peer netip.Addr, trusted []netip.Prefix) (netip.Addr, bool) {
	if !isTrusted(peer.Unmap(), trusted) {
		return netip.Addr{}, false
	}
	var chain []netip.Addr
	for _, v := range h.Values("X-Forwarded-For") {
		for _, part := range strings.Split(v, ",") {
			if a, ok := parseHop(part); ok {
				chain = append(chain, a)
			}
		}
	}
	// nginx sets X-Real-IP to the peer it saw ($remote_addr), normally equal to the last XFF hop.
	if a, ok := parseHop(h.Get("X-Real-IP")); ok && (len(chain) == 0 || chain[len(chain)-1] != a) {
		chain = append(chain, a)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !isTrusted(chain[i], trusted) {
			return chain[i], true
		}
	}
	// Every hop was one of our proxies (e.g. tunnel -> nginx): Cloudflare's header names the visitor.
	if len(chain) > 0 {
		if a, ok := parseHop(h.Get("CF-Connecting-IP")); ok {
			return a, true
		}
	}
	return netip.Addr{}, false
}
