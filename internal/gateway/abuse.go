package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// errFreeTierIPBlocked reports that OpenCode's free gateway refused the request
// because of the egress IP, not the request, the key or the session: it either
// flagged the IP or the IP's daily free allowance is spent. Unlike the shape
// gates, another lane, key or retry cannot fix it: the same IP gets the same
// answer, and retrying only extends the penalty. The request must therefore
// leave the anonymous lane instead of walking its upstreams, and the exit must
// stay out of the cursor until the block or the quota window expires.
var errFreeTierIPBlocked = errors.New("free tier blocked the egress IP")

// freeTierBlockedMessage is what a client receives while every exit it could
// reach is retired. The upstream message tells people to turn off a VPN, which
// is wrong for a server that egresses through a proxy; this one names the real
// causes and the ways out.
const freeTierBlockedMessage = "Every exit this gateway could use has spent its OpenCode free-model quota or is blocked by IP, so retrying now cannot succeed. Add exits with fresh IPs, wait for the daily quota reset (around 00:00 UTC), or configure an authenticated key."

// errAnonymousLanePaused reports that the anonymous lane's circuit breaker has
// paused the whole lane. Unlike an upstream rejection, another exit or a retry
// cannot succeed while the lane is paused; the caller should use an
// authenticated tier or report the pause itself.
var errAnonymousLanePaused = errors.New("anonymous lane paused by its circuit breaker")

// anonymousLanePausedMessage is what a client receives while the circuit
// breaker holds the anonymous lane shut. It names the cause so the operator is
// not sent looking for a credential or model problem.
const anonymousLanePausedMessage = "The anonymous free tier is paused after repeated upstream failures. Configure an authenticated key, wait for the pause to expire, or restart the gateway after confirming the upstream block has lifted."

// freeTierBlockTTL keeps a blocked exit out of the anonymous cursor. Upstream
// does not publish the block duration, so this is a conservative backoff: long
// enough to stop the retry storm, short enough that an unblocked IP recovers
// without a restart.
const freeTierBlockTTL = 15 * time.Minute

// freeTierBlockTTL returns the backoff applied to an exit the free tier flagged.
func (g *Gateway) freeTierBlockTTL() time.Duration {
	if g != nil && g.freeTierBlock > 0 {
		return g.freeTierBlock
	}
	return freeTierBlockTTL
}

// maxFreeUsageBlockTTL bounds how long a FreeUsageLimitError keeps an exit out
// of rotation. The upstream's Retry-After is commonly hours (57733s on
// 2026-11-04), far past the abuse block's window; anything above a day is
// treated as a day so a misreported header cannot retire the pool forever.
const maxFreeUsageBlockTTL = 24 * time.Hour

// freeUsageBlockTTL is the retirement window for an exit whose free allowance
// is spent. It honors the upstream Retry-After and falls back to the standard
// abuse block when the header is missing or unreadable.
func (g *Gateway) freeUsageBlockTTL(retryAfter string) time.Duration {
	if d := parseRetryAfter(retryAfter); d > 0 {
		return min(d, maxFreeUsageBlockTTL)
	}
	return g.freeTierBlockTTL()
}

// abuseProbeLimit bounds how much of a 403 body is read to classify it.
const abuseProbeLimit = 16 << 10

// isNetworkAbuseBlock reports whether status and body are the upstream's IP
// abuse rejection. The response is a 403 whose message names the egress
// network, in one of the wordings the gateway has been served:
//
//   - "…free models aren't available from your current network because we've
//     detected abusive traffic from this IP address…"
//   - "Forbidden, OpenCode Free Model Gateway Detect Abuse Traffic"
//   - "Abuse of traffic"
//
// The shape gate's 403 ("free tier can only be used from within OpenCode") is
// deliberately not matched: that one is a request-shape problem, which another
// lane or a refreshed body can still fix.
func isNetworkAbuseBlock(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(string(body))
	if strings.Contains(text, "abusive traffic") ||
		strings.Contains(text, "abuse traffic") ||
		strings.Contains(text, "abuse of traffic") {
		return true
	}
	return strings.Contains(text, "free models aren't available") &&
		strings.Contains(text, "from your current network")
}

// isFreeUsageLimitBody reports the free tier's quota refusal, HTTP 429
// {"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage
// limit reached..."}}. That limit is metered per egress address for hours, not
// per conversation session, so rotating the session cannot clear it.
func isFreeUsageLimitBody(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "freeusagelimiterror") ||
		strings.Contains(text, "free usage limit reached")
}

// peekResponseBody reads the start of a response body and puts it back in
// front of the rest, so classification and downstream error handling both see
// the full payload.
func peekResponseBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, abuseProbeLimit))
	resp.Body = replayBody(prefix, resp.Body)
	if err != nil {
		return nil
	}
	return prefix
}

// peekNetworkAbuseBlock classifies a response without consuming its body.
func peekNetworkAbuseBlock(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	return isNetworkAbuseBlock(resp.StatusCode, peekResponseBody(resp))
}

// errDeterministicClientRejection reports that the upstream rejected the
// request itself: the body is malformed, too long for the model, or fails the
// same validation on every credential. Retrying on another key, lane or tier
// cannot change that, so the request must end and the original 4xx must reach
// the client. Without this, the first tier's real error is replaced by
// whatever the next tier answers — typically a 401 from an unconfigured key,
// which tells the client to re-authenticate instead of compacting.
var errDeterministicClientRejection = errors.New("upstream rejected the request deterministically")

// deterministicMarkers are the error types and codes an upstream uses for a
// request that will fail identically everywhere. The status must be 400 or 422
// as well: a 401, 403 or 429 can be specific to the credential or the moment.
var deterministicMarkers = []string{
	"invalid_request_error",
	"invalid_request",
	"context_length_exceeded",
	"maximum context length",
	"string_above_max_length",
	"prompt is too long",
}

// isDeterministicClientResponse reports whether status and body describe a
// request-level rejection rather than a credential or availability problem.
func isDeterministicClientResponse(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	text := strings.ToLower(string(body))
	for _, marker := range deterministicMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// replayBody returns a body that yields prefix and then the rest of original.
func replayBody(prefix []byte, original io.ReadCloser) io.ReadCloser {
	return &prefixBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original}
}

type prefixBody struct {
	io.Reader
	closer io.Closer
}

func (b *prefixBody) Close() error { return b.closer.Close() }
