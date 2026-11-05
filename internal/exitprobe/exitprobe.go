// Package exitprobe measures one outbound exit through an existing local
// SOCKS listener: which public IP it presents and whether that IP still has
// free-model quota. The request mirrors the gateway's v2 /inference call so a
// passing probe is a real end-to-end check, not a reachability ping.
package exitprobe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
)

// DialFunc opens a TCP connection through an exit. The admin server supplies
// one that speaks SOCKS5 to a local listener.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Quota states reported for an exit.
const (
	QuotaOK      = "ok"
	QuotaLimited = "limited"
	QuotaBlocked = "blocked"
	QuotaError   = "error"
	QuotaUnknown = "unknown"
)

// Default endpoints. The inference URL is the exact route the anonymous tier
// uses: /inference + /openai + /v1/chat/completions.
const (
	DefaultInferenceURL = "https://opencode.ai/inference/openai/v1/chat/completions"
	DefaultIPURL        = "https://api.ipify.org?format=text"
	FallbackIPURL       = "https://cloudflare.com/cdn-cgi/trace"
	AnonymousKey        = "public"
)

// Options controls one probe. Zero values fall back to the defaults.
type Options struct {
	Model        string
	Timeout      time.Duration
	InferenceURL string
	IPURL        string
	FallbackURL  string
}

// Result is the outcome stored on the node and shown in the panel.
type Result struct {
	EgressIP          string `json:"egress_ip,omitempty"`
	Quota             string `json:"quota"`
	StatusCode        int    `json:"status_code,omitempty"`
	RetryAfterSeconds int64  `json:"retry_after_seconds,omitempty"`
	LatencyMs         int64  `json:"latency_ms,omitempty"`
	Note              string `json:"note,omitempty"`
	CheckedAt         string `json:"checked_at"`
}

// Probe runs the egress-IP lookup and the free-model request through dial.
func Probe(ctx context.Context, dial DialFunc, opts Options) Result {
	res := Result{Quota: QuotaUnknown, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if dial == nil {
		res.Quota, res.Note = QuotaError, "没有可用的出口拨号器"
		return res
	}
	if opts.Model == "" {
		opts.Model = "beam-free"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.InferenceURL == "" {
		opts.InferenceURL = DefaultInferenceURL
	}
	if opts.IPURL == "" {
		opts.IPURL = DefaultIPURL
	}
	if opts.FallbackURL == "" {
		opts.FallbackURL = FallbackIPURL
	}
	transport := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: opts.Timeout,
	}
	client := &http.Client{Transport: transport, Timeout: opts.Timeout}
	defer transport.CloseIdleConnections()

	res.EgressIP = lookupIP(ctx, client, opts)
	started := time.Now()
	status, header, body, err := inference(ctx, client, opts)
	res.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		res.Quota = QuotaError
		res.Note = trim(err.Error(), 180)
		return res
	}
	res.StatusCode = status
	res.Quota, res.RetryAfterSeconds, res.Note = classify(status, header, body)
	return res
}

// lookupIP asks a public echo service which address the exit presents. A
// failure is not fatal: the quota answer is still useful without the IP.
func lookupIP(ctx context.Context, client *http.Client, opts Options) string {
	for _, endpoint := range []string{opts.IPURL, opts.FallbackURL} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", httpx.V2UserAgent())
		req.Header.Set("Accept", "text/plain, */*")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		if ip := parseIPBody(string(data)); ip != "" {
			return ip
		}
	}
	return ""
}

func parseIPBody(body string) string {
	body = strings.TrimSpace(body)
	if ip := net.ParseIP(body); ip != nil {
		return ip.String()
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ip=") {
			if ip := net.ParseIP(strings.TrimPrefix(line, "ip=")); ip != nil {
				return ip.String()
			}
		}
	}
	return ""
}

// inference sends the same minimal request the gateway sends for a free
// model, with the v2 gate's exact headers.
func inference(ctx context.Context, client *http.Client, opts Options) (int, http.Header, []byte, error) {
	payload := map[string]any{
		"model": opts.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with exactly: OK"},
		},
		"max_tokens": 64,
		"stream":     true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.InferenceURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, nil, err
	}
	session := identity.CanonicalSessionID(identity.RandomID("egress", 16))
	project := identity.RandomProjectID()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+AnonymousKey)
	req.Header.Set("User-Agent", httpx.V2UserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("X-Session-Id", session)
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-opencode-project", project)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, resp.Header, data, nil
}

// classify maps the upstream answer onto the quota state. The free tier
// reports an exhausted IP with 429 FreeUsageLimitError, and a flagged network
// with 403 abuse wording.
func classify(status int, header http.Header, body []byte) (string, int64, string) {
	text := strings.ToLower(string(body))
	switch {
	case status >= 200 && status < 300:
		return QuotaOK, 0, ""
	case status == http.StatusTooManyRequests || strings.Contains(text, "freeusagelimiterror") || strings.Contains(text, "free usage limit"):
		retry := parseRetryAfter(header.Get("Retry-After"))
		return QuotaLimited, retry, trim(string(body), 200)
	case status == http.StatusForbidden && (strings.Contains(text, "abusive traffic") ||
		strings.Contains(text, "abuse traffic") || strings.Contains(text, "abuse of traffic") ||
		strings.Contains(text, "free models aren't available") || strings.Contains(text, "current network")):
		return QuotaBlocked, parseRetryAfter(header.Get("Retry-After")), trim(string(body), 200)
	case status == http.StatusUnauthorized:
		return QuotaError, 0, "上游拒绝了匿名凭据 (401)"
	case status >= 500:
		return QuotaError, 0, fmt.Sprintf("上游错误 HTTP %d", status)
	default:
		return QuotaError, 0, fmt.Sprintf("HTTP %d: %s", status, trim(string(body), 160))
	}
}

func parseRetryAfter(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		if seconds := int64(time.Until(when).Seconds()); seconds > 0 {
			return seconds
		}
	}
	return 0
}

func trim(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
