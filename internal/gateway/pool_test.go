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

// A per-model hold keeps the exit out of that model's cursor only: beam
// still draws it while deepseek skips it, and a cursor with no model is
// unaffected.
func TestPrescreenModelLimitedSkipsOnlyThatModel(t *testing.T) {
	burned := testStaticNode("socks5://127.0.0.1:21000")
	live := testStaticNode("socks5://127.0.0.1:21001")
	pool := testPool(burned, live)
	now := time.Now()

	pool.MarkModelLimited(burned, "deepseek-v4.1-flash-free", now.Add(time.Hour))

	deepseekCursor := pool.CursorForModel("s", "deepseek-v4.1-flash-free")
	if got := deepseekCursor.Next(); got != live {
		t.Fatalf("deepseek cursor = %v, want the live node only", got)
	}
	beamCursor := pool.CursorForModel("s", "beam-free")
	if got := beamCursor.Next(); got == nil {
		t.Fatal("beam cursor returned nil, want a node (per-model hold must not block other models)")
	}
	plainCursor := pool.CursorFor("s")
	if got := plainCursor.Next(); got == nil {
		t.Fatal("model-less cursor returned nil, want a node")
	}
	if got := pool.AvailableExits(now); got != 2 {
		t.Fatalf("AvailableExits = %d, want 2 (per-model holds do not change the global count)", got)
	}
}

// A per-model hold expires on its own: once it is cleared (the way a success
// for that model clears it), the model draws the exit again.
func TestPrescreenModelLimitClears(t *testing.T) {
	node := testStaticNode("socks5://127.0.0.1:21000")
	pool := testPool(node)
	now := time.Now()

	pool.MarkModelLimited(node, "deepseek-v4.1-flash-free", now.Add(time.Hour))
	heldCursor := pool.CursorForModel("s", "deepseek-v4.1-flash-free")
	if got := heldCursor.Next(); got != nil {
		t.Fatalf("deepseek cursor returned a node while held, want nil")
	}
	pool.clearModelLimited(node, "deepseek-v4.1-flash-free")
	freedCursor := pool.CursorForModel("s", "deepseek-v4.1-flash-free")
	if got := freedCursor.Next(); got != node {
		t.Fatalf("deepseek cursor after clear = %v, want the node", got)
	}
}

// A per-model hold is a no-op for #pool nodes (the listener draws a fresh
// egress per connection, so it is never spent) and for an empty model.
func TestPrescreenModelLimitedIgnoresPoolNodes(t *testing.T) {
	pooled := &proxyTransport{name: "socks5://127.0.0.1:21002", pool: true}
	pooled.healthy.Store(true)
	pool := testPool(&anonymousNode{proxy: pooled})
	now := time.Now()

	pool.MarkModelLimited(pool.nodes[0], "deepseek-v4.1-flash-free", now.Add(time.Hour))
	poolCursor := pool.CursorForModel("s", "deepseek-v4.1-flash-free")
	if got := poolCursor.Next(); got == nil {
		t.Fatal("deepseek cursor returned nil for a #pool node, want the node (pool nodes are never held)")
	}
}

// prescreenBlockTTL honors a Retry-After (capped at a day); without one it
// holds until the next UTC midnight, when the daily quota resets.
func TestPrescreenBlockTTL(t *testing.T) {
	if got := prescreenBlockTTL("3600"); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("prescreenBlockTTL(3600) = %v, want ~1h", got)
	}
	if got := prescreenBlockTTL("999999"); got > 24*time.Hour {
		t.Fatalf("prescreenBlockTTL(999999) = %v, want at most 24h", got)
	}
	before := time.Now().UTC()
	got := prescreenBlockTTL("")
	next := time.Date(before.Year(), before.Month(), before.Day()+1, 0, 0, 0, 0, time.UTC)
	want := time.Until(next)
	if got < want-2*time.Second || got > want+2*time.Second {
		t.Fatalf("prescreenBlockTTL(\"\") = %v, want ~%v (until next UTC midnight)", got, want)
	}
	if got <= 0 || got > 24*time.Hour {
		t.Fatalf("prescreenBlockTTL(\"\") = %v, want (0, 24h]", got)
	}
}
