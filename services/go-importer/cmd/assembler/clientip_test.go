package main

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestRealClientIP(t *testing.T) {
	// nginx 192.168.48.9 and cloudflared 192.168.48.10 are our proxies
	trusted := ParseTrustedProxies("192.168.48.9, 192.168.48.10,10.99.0.0/16, bogus")
	nginx := "192.168.48.9"

	cases := []struct {
		name    string
		peer    string // flow source (who sent us the packets)
		headers map[string]string
		want    string // "" = no tag (peer itself is the client, or nothing better)
	}{
		{"no proxy headers", nginx, map[string]string{}, ""},
		{"direct to nginx", nginx, map[string]string{"X-Forwarded-For": "10.60.3.1", "X-Real-IP": "10.60.3.1"}, "10.60.3.1"},
		{"forged XFF is ignored", nginx, map[string]string{"X-Forwarded-For": "6.6.6.6, 10.60.3.1", "X-Real-IP": "10.60.3.1"}, "10.60.3.1"},
		{"via tunnel skips cloudflared", nginx, map[string]string{"X-Forwarded-For": "203.0.113.5, 192.168.48.10", "X-Real-IP": "192.168.48.10"}, "203.0.113.5"},
		{"forged XFF via tunnel", nginx, map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.5, 192.168.48.10", "X-Real-IP": "192.168.48.10"}, "203.0.113.5"},
		{"all trusted, CF header", nginx, map[string]string{"X-Real-IP": "192.168.48.10", "CF-Connecting-IP": "198.51.100.7"}, "198.51.100.7"},
		{"all trusted, no CF header", nginx, map[string]string{"X-Real-IP": "192.168.48.10"}, ""},
		{"CF header alone is not trusted", nginx, map[string]string{"CF-Connecting-IP": "6.6.6.6"}, ""},
		{"only X-Real-IP", nginx, map[string]string{"X-Real-IP": "10.60.7.2"}, "10.60.7.2"},
		{"ip:port and garbage", nginx, map[string]string{"X-Forwarded-For": "junk, 10.60.3.1:51234"}, "10.60.3.1"},
		{"ipv6 with port", nginx, map[string]string{"X-Forwarded-For": "[2001:db8::1]:443"}, "2001:db8::1"},
		{"trusted CIDR", nginx, map[string]string{"X-Forwarded-For": "10.60.3.1, 10.99.4.4"}, "10.60.3.1"},
		{"docker gateway is not trusted", nginx, map[string]string{"X-Forwarded-For": "6.6.6.6, 192.168.48.1", "X-Real-IP": "192.168.48.1"}, "192.168.48.1"},
		// The attacker talks to nginx itself: every header is theirs, they are the flow source already.
		{"untrusted peer forging XFF", "192.168.48.1", map[string]string{"X-Forwarded-For": "6.6.6.6"}, ""},
		{"untrusted peer forging everything", "10.60.3.1", map[string]string{"X-Forwarded-For": "6.6.6.6", "X-Real-IP": "6.6.6.6", "CF-Connecting-IP": "6.6.6.6"}, ""},
		{"no trusted proxies configured", nginx, nil, ""},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		tr := trusted
		if c.headers == nil {
			tr = nil
			h.Set("X-Forwarded-For", "6.6.6.6")
		}
		got, ok := realClientIP(h, netip.MustParseAddr(c.peer), tr)
		gotS := ""
		if ok {
			gotS = got.String()
		}
		if gotS != c.want {
			t.Errorf("%s: got %q, want %q", c.name, gotS, c.want)
		}
	}
}

func TestWantSrcTag(t *testing.T) {
	vpn := ParseTrustedProxies("10.60.0.0/16,10.61.0.0/16")
	cases := []struct {
		ip     string
		ranges []netip.Prefix
		want   bool
	}{
		{"10.60.3.1", vpn, true},
		{"10.61.200.9", vpn, true},
		{"192.168.48.1", vpn, false}, // Docker gateway
		{"192.168.0.10", vpn, false}, // LAN PC
		{"::ffff:10.60.3.1", vpn, true},
		{"192.168.0.10", nil, true}, // no VPN range configured: tag everything
	}
	for _, c := range cases {
		if got := wantSrcTag(netip.MustParseAddr(c.ip), c.ranges); got != c.want {
			t.Errorf("wantSrcTag(%s): got %v, want %v", c.ip, got, c.want)
		}
	}
}
