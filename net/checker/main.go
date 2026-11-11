package main

// checker - small HTTP health-check service on :8199.
//
// The original container (1c/512M, volume /data, port 8199) had no source
// binding on InstaCloud, so this is a clean rebuild of the same contract:
//
//	GET  /healthz                      service liveness
//	GET  /api/check?target=HOST:PORT   check one target (tcp/http/https)
//	POST /api/check                    check a batch of targets
//
// Every completed run is appended to $STATE_DIR/checks.jsonl.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "1.0.0"

type server struct {
	listen     string
	token      string
	stateDir   string
	maxBatch   int
	defaultTO  time.Duration
	httpClient *http.Client
	startedAt  time.Time
}

type checkRequest struct {
	Targets      []string `json:"targets"`
	Target       string   `json:"target"`
	Scheme       string   `json:"scheme"`
	Path         string   `json:"path"`
	TimeoutMS    int      `json:"timeout_ms"`
	ExpectStatus int      `json:"expect_status"`
	Contains     string   `json:"contains"`
	Concurrency  int      `json:"concurrency"`
}

type checkResult struct {
	Target    string `json:"target"`
	Scheme    string `json:"scheme"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type checkResponse struct {
	OK      bool          `json:"ok"`
	Total   int           `json:"total"`
	Passed  int           `json:"passed"`
	Failed  int           `json:"failed"`
	Results []checkResult `json:"results"`
}

type healthResponse struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptime_s"`
	Targets   int    `json:"targets_checked_total"`
}

var (
	countMu    sync.Mutex
	checkedAll int64
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		listenFlag  = flag.String("listen", "", "listen address (default :8199)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	s := &server{
		listen:    firstNonEmpty(*listenFlag, envOr("LISTEN", ":8199")),
		token:     strings.TrimSpace(os.Getenv("CHECKER_TOKEN")),
		stateDir:  envOr("STATE_DIR", "/data"),
		maxBatch:  envInt("MAX_BATCH", 64),
		defaultTO: time.Duration(envInt("DEFAULT_TIMEOUT_MS", 5000)) * time.Millisecond,
		startedAt: time.Now(),
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		log.Printf("warning: cannot create state dir %s: %v", s.stateDir, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/check", s.auth(s.handleCheck))
	mux.HandleFunc("/api/info", s.auth(s.handleInfo))

	srv := &http.Server{
		Addr:              s.listen,
		Handler:           withCORS(withLog(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("checker %s listening on %s (state=%s, auth=%v)", version, s.listen, s.stateDir, s.token != "")
	log.Fatal(srv.ListenAndServe())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Checker-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimSpace(r.Header.Get("X-Checker-Token"))
			if got == "" {
				h := strings.TrimSpace(r.Header.Get("Authorization"))
				if strings.HasPrefix(strings.ToLower(h), "bearer ") {
					got = strings.TrimSpace(h[7:])
				}
			}
			if got != s.token {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	countMu.Lock()
	n := checkedAll
	countMu.Unlock()
	writeJSON(w, http.StatusOK, healthResponse{
		OK:        true,
		Version:   version,
		UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		Targets:   int(n),
	})
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"version":            version,
		"listen":             s.listen,
		"state_dir":          s.stateDir,
		"max_batch":          s.maxBatch,
		"default_timeout_ms": int(s.defaultTO.Milliseconds()),
		"auth":               s.token != "",
	})
}

func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		req.Target = q.Get("target")
		req.Scheme = q.Get("scheme")
		req.Path = q.Get("path")
		req.Contains = q.Get("contains")
		if v := q.Get("timeout_ms"); v != "" {
			req.TimeoutMS, _ = strconv.Atoi(v)
		}
		if v := q.Get("expect_status"); v != "" {
			req.ExpectStatus, _ = strconv.Atoi(v)
		}
		req.Concurrency = 1
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read body: " + err.Error()})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json: " + err.Error()})
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	targets := req.Targets
	if strings.TrimSpace(req.Target) != "" {
		targets = append(targets, req.Target)
	}
	targets = normalizeTargets(targets)
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target or targets required"})
		return
	}
	if len(targets) > s.maxBatch {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("too many targets: %d > MAX_BATCH=%d", len(targets), s.maxBatch),
		})
		return
	}

	scheme := strings.ToLower(strings.TrimSpace(req.Scheme))
	if scheme == "" {
		scheme = "tcp"
	}
	switch scheme {
	case "tcp", "http", "https":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "scheme must be tcp, http or https"})
		return
	}

	timeout := s.defaultTO
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	path := req.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	conc := req.Concurrency
	if conc <= 0 {
		conc = 8
	}
	if conc > len(targets) {
		conc = len(targets)
	}
	if conc > 32 {
		conc = 32
	}

	results := make([]checkResult, len(targets))
	var wg sync.WaitGroup
	idx := make(chan int)
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				results[i] = s.checkOne(targets[i], scheme, path, timeout, req.ExpectStatus, req.Contains)
			}
		}()
	}
	for i := range targets {
		idx <- i
	}
	close(idx)
	wg.Wait()

	resp := checkResponse{Total: len(results), Results: results}
	for _, r := range results {
		if r.OK {
			resp.Passed++
		} else {
			resp.Failed++
		}
	}
	resp.OK = resp.Failed == 0
	s.record(targets, scheme, resp)
	writeJSON(w, http.StatusOK, resp)
}

func normalizeTargets(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func (s *server) checkOne(target, scheme, path string, timeout time.Duration, expectStatus int, contains string) checkResult {
	res := checkResult{Target: target, Scheme: scheme}
	if scheme == "tcp" {
		if _, _, err := net.SplitHostPort(target); err != nil {
			target = net.JoinHostPort(target, "443")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()

	if scheme == "tcp" {
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", target)
		res.LatencyMS = time.Since(start).Milliseconds()
		if err != nil {
			res.Error = err.Error()
			return res
		}
		_ = conn.Close()
		res.OK = true
		return res
	}

	url := scheme + "://" + strings.TrimSuffix(target, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "o2a-checker/"+version)
	client := *s.httpClient
	client.Timeout = timeout
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	ok := true
	if expectStatus > 0 {
		ok = resp.StatusCode == expectStatus
	} else {
		ok = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	if ok && contains != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		if err != nil {
			res.Error = err.Error()
			return res
		}
		ok = strings.Contains(string(body), contains)
		if !ok {
			res.Error = "body does not contain " + strconv.Quote(contains)
		}
	}
	res.OK = ok
	if !ok && res.Error == "" {
		res.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return res
}

func (s *server) record(targets []string, scheme string, resp checkResponse) {
	countMu.Lock()
	checkedAll += int64(len(targets))
	countMu.Unlock()

	entry := map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
		"scheme":  scheme,
		"total":   resp.Total,
		"passed":  resp.Passed,
		"failed":  resp.Failed,
		"results": resp.Results,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.stateDir, "checks.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("record: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
