package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testStaticNode(name string) *anonymousNode {
	tr := &proxyTransport{name: name}
	tr.healthy.Store(true)
	return &anonymousNode{proxy: tr}
}

func test429(retryAfter, body string) *http.Response {
	header := http.Header{}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testPool(nodes ...*anonymousNode) *anonymousNodes {
	return &anonymousNodes{
		nodes:             nodes,
		cooldown:          time.Minute,
		rateLimitCooldown: 15 * time.Minute,
		breaker:           newLaneBreaker(anonymousBackoff{threshold: 2, base: time.Minute, max: time.Hour}),
	}
}

// A quota 429 must hold the exit for the configured hard window even when the
// upstream advertises a much longer Retry-After (the header is scoped to the
// current egress address, which the external producer re-rolls on its own
// schedule). It must not count as a lane failure either.
func TestQuota429HoldsFullWindowAndIgnoresRetryAfter(t *testing.T) {
	node := testStaticNode("socks5://127.0.0.1:21000")
	pool := testPool(node)
	before := time.Now()

	pool.MarkFailure(node, test429("61500", `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`), nil)

	hold := time.Unix(0, node.limitedUntil.Load()).Sub(before)
	if hold < 14*time.Minute+50*time.Second || hold > 15*time.Minute+10*time.Second {
		t.Fatalf("quota hold = %v, want ~15m regardless of Retry-After", hold)
	}
	if node.failures.Load() != 0 {
		t.Fatalf("quota 429 counted %d lane failures", node.failures.Load())
	}
	if pool.breaker.Open() {
		t.Fatal("quota 429 tripped the lane breaker")
	}
	if got := pool.AvailableExits(time.Now()); got != 0 {
		t.Fatalf("AvailableExits = %d while held, want 0", got)
	}
}

// A transient 429 may advertise a shorter retry window; only that shorter
// value is adopted, never a longer one.
func TestTransient429AdoptsShorterRetryAfter(t *testing.T) {
	node := testStaticNode("socks5://127.0.0.1:21000")
	pool := testPool(node)
	before := time.Now()

	pool.MarkFailure(node, test429("20", `{"error":{"type":"rate_limit_exceeded"}}`), nil)

	hold := time.Unix(0, node.limitedUntil.Load()).Sub(before)
	if hold < 15*time.Second || hold > 25*time.Second {
		t.Fatalf("transient hold = %v, want ~20s", hold)
	}
}

// AvailableExits counts only exits a static draw can use: the host direct
// route and unhealthy transports are excluded, a held or cooling plain node is
// skipped, and a rotating #pool transport stays counted.
func TestAvailableExitsCountsDrawableStaticNodes(t *testing.T) {
	live := testStaticNode("socks5://127.0.0.1:21000")
	held := testStaticNode("socks5://127.0.0.1:21001")
	direct := &proxyTransport{name: "direct"}
	direct.healthy.Store(true)
	pooled := &proxyTransport{name: "socks5://127.0.0.1:21002", pool: true}
	pooled.healthy.Store(true)
	unhealthy := &proxyTransport{name: "socks5://127.0.0.1:21003"}

	pool := testPool(live, held, &anonymousNode{proxy: direct}, &anonymousNode{proxy: pooled}, &anonymousNode{proxy: unhealthy})
	now := time.Now()
	held.limitedUntil.Store(now.Add(15 * time.Minute).UnixNano())

	if got := pool.AvailableExits(now); got != 2 {
		t.Fatalf("AvailableExits = %d, want 2 (live and #pool)", got)
	}
	live.cooldownUntil.Store(now.Add(time.Minute).UnixNano())
	if got := pool.AvailableExits(now); got != 1 {
		t.Fatalf("AvailableExits = %d after soft cooldown, want 1 (#pool only)", got)
	}

	if !pool.HasExternal() {
		t.Fatal("HasExternal = false with proxy exits configured")
	}
	onlyDirect := testPool(&anonymousNode{proxy: direct})
	if onlyDirect.HasExternal() {
		t.Fatal("HasExternal = true for a direct-only pool")
	}
}
