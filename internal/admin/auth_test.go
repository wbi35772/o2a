package admin

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func setTrustedProxies(t *testing.T, prefixes ...netip.Prefix) {
	t.Helper()
	previous := trustedProxyPrefixes
	trustedProxyPrefixes = prefixes
	t.Cleanup(func() { trustedProxyPrefixes = previous })
}

func TestClientIPUsesForwardedForBehindTrustedProxy(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("10.0.0.0/8"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want the forwarded client", got)
	}
}

func TestClientIPUsesRightmostUntrustedHop(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("10.0.0.0/8"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9, 10.0.0.5")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want the rightmost untrusted hop", got)
	}
}

func TestClientIPFallsBackToLeftmostWhenEveryHopIsTrusted(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("10.0.0.0/8"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "10.1.1.1, 10.0.0.5")
	if got := clientIP(request); got != "10.1.1.1" {
		t.Fatalf("clientIP = %q, want the leftmost hop", got)
	}
}

func TestClientIPIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("10.0.0.0/8"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "198.51.100.7:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(request); got != "198.51.100.7" {
		t.Fatalf("clientIP = %q, want the direct peer", got)
	}
}

func TestClientIPFallsBackWhenForwardedForIsMalformed(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("10.0.0.0/8"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "10.0.0.5:4321"
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(request); got != "10.0.0.5" {
		t.Fatalf("clientIP = %q, want the direct peer", got)
	}
}

func TestClientIPHandlesIPv6(t *testing.T) {
	setTrustedProxies(t, netip.MustParsePrefix("::1/128"))
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.RemoteAddr = "[::1]:4321"
	request.Header.Set("X-Forwarded-For", "2001:db8::7")
	if got := clientIP(request); got != "2001:db8::7" {
		t.Fatalf("clientIP = %q, want the forwarded IPv6 client", got)
	}
}

func TestParseTrustedProxyPrefixes(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"10.0.0.0/8, 192.168.1.7", []string{"10.0.0.0/8", "192.168.1.7/32"}},
		{"none", nil},
		{"", nil},
		{"bad, 127.0.0.1", []string{"127.0.0.1/32"}},
	}
	for _, test := range cases {
		got := parseTrustedProxyPrefixes(test.raw)
		if len(got) != len(test.want) {
			t.Fatalf("parseTrustedProxyPrefixes(%q) = %v, want %v", test.raw, got, test.want)
		}
		for i := range got {
			if got[i].String() != test.want[i] {
				t.Fatalf("parseTrustedProxyPrefixes(%q)[%d] = %s, want %s", test.raw, i, got[i], test.want[i])
			}
		}
	}
}

func newTestServer() *Server {
	return &Server{
		sessions:          make(map[string]adminSession),
		attempts:          make(map[string]loginWindow),
		debugAttempts:     make(map[string]loginWindow),
		sensitiveAttempts: make(map[string]loginWindow),
	}
}

func TestAllowLoginBlocksAfterFiveFailures(t *testing.T) {
	server := newTestServer()
	for i := 0; i < 5; i++ {
		if !server.allowLogin("203.0.113.9") {
			t.Fatalf("attempt %d rejected before the limit", i+1)
		}
		server.recordLoginFailure("203.0.113.9")
	}
	if server.allowLogin("203.0.113.9") {
		t.Fatal("sixth attempt was allowed")
	}
	if !server.allowLogin("198.51.100.7") {
		t.Fatal("a different client shared the window")
	}
}

func TestAllowLoginResetsAfterWindow(t *testing.T) {
	server := newTestServer()
	server.attempts["203.0.113.9"] = loginWindow{Started: time.Now().Add(-6 * time.Minute), Count: 5}
	if !server.allowLogin("203.0.113.9") {
		t.Fatal("a stale window was not reset")
	}
}

func TestAllowSensitiveLimitsEveryAttempt(t *testing.T) {
	server := newTestServer()
	for i := 0; i < 5; i++ {
		if !server.allowSensitive("203.0.113.9") {
			t.Fatalf("attempt %d rejected before the limit", i+1)
		}
	}
	if server.allowSensitive("203.0.113.9") {
		t.Fatal("sixth sensitive attempt was allowed")
	}
	if !server.allowSensitive("198.51.100.7") {
		t.Fatal("a different client shared the window")
	}
}

func TestAllowSensitiveResetsAfterWindow(t *testing.T) {
	server := newTestServer()
	server.sensitiveAttempts["203.0.113.9"] = loginWindow{Started: time.Now().Add(-6 * time.Minute), Count: 5}
	if !server.allowSensitive("203.0.113.9") {
		t.Fatal("a stale window was not reset")
	}
}
