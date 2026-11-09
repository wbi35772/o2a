package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// upstreamSpec is one pool the hub aggregates. The file is re-read on every
// (re)build so operators can add or remove pools without restarting socksmux.
type upstreamSpec struct {
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	TokenEnv     string `json:"token_env,omitempty"`
	Token        string `json:"token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	OverrideHost string `json:"override_host,omitempty"`
	OverridePort int    `json:"override_port,omitempty"`
	SocksUser    string `json:"socks_user,omitempty"`
	SocksPassEnv string `json:"socks_pass_env,omitempty"`
	SocksPass    string `json:"socks_pass,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

func (u upstreamSpec) isEnabled() bool { return u.Enabled == nil || *u.Enabled }

func (u upstreamSpec) scope() string {
	if strings.EqualFold(u.Scope, "public") {
		return "public"
	}
	return "private"
}

type mergedUpstreamView struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Nodes     int    `json:"nodes_total"`
	NodesOK   int    `json:"nodes_ok"`
	Unique64  int    `json:"unique64"`
	LatencyMS int64  `json:"latency_ms"`
}

type mergedNode struct {
	Prefix64 string `json:"prefix64"`
	Upstream string `json:"upstream"`
	User     string `json:"user"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Loc      string `json:"loc,omitempty"`
	URL      string `json:"url,omitempty"`
}

type mergedSnapshot struct {
	GeneratedAt string               `json:"generated_at"`
	Unique64    int                  `json:"unique64"`
	Endpoints   int                  `json:"endpoints"`
	Upstreams   []mergedUpstreamView `json:"upstreams"`
	Nodes       []mergedNode         `json:"nodes"`
}

type upstreamStatusDoc struct {
	Pool struct {
		ID          string `json:"id"`
		PrivateHost string `json:"private_host"`
		PublicHost  string `json:"public_host"`
		MuxPort     int    `json:"mux_port"`
		Count       int    `json:"count"`
		Healthy     int    `json:"healthy"`
	} `json:"pool"`
	Nodes []struct {
		User     string `json:"user"`
		Port     int    `json:"port"`
		State    string `json:"state"`
		Enabled  bool   `json:"enabled"`
		Prefix64 string `json:"prefix64"`
		Loc      string `json:"loc"`
	} `json:"nodes"`
}

// mergedHub fetches every configured pool, dedupes by /64 and exposes one
// stable endpoint list. Selection is deterministic: for a given prefix the
// smallest (upstream name, user) pair always wins, so consumer reloads do not
// flap when two pools advertise the same prefix.
type mergedHub struct {
	path        string
	defaultUser string
	passLookup  func(upstreamSpec) string

	mu      sync.Mutex
	cache   *mergedSnapshot
	cacheAt time.Time
	ttl     time.Duration
}

func newMergedHub(stateDir, file, defaultUser string, passLookup func(upstreamSpec) string) *mergedHub {
	if strings.TrimSpace(file) == "" {
		file = filepath.Join(stateDir, "control", "upstreams.json")
	}
	return &mergedHub{
		path:        file,
		defaultUser: defaultUser,
		passLookup:  passLookup,
		ttl:         5 * time.Second,
	}
}

func (h *mergedHub) loadSpecs() ([]upstreamSpec, error) {
	b, err := os.ReadFile(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("upstreams file %s not found", h.path)
		}
		return nil, err
	}
	var specs []upstreamSpec
	if err := json.Unmarshal(b, &specs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", h.path, err)
	}
	out := make([]upstreamSpec, 0, len(specs))
	for _, s := range specs {
		if !s.isEnabled() {
			continue
		}
		s.Name = strings.TrimSpace(s.Name)
		s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if s.Name == "" || s.BaseURL == "" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (h *mergedHub) tokenFor(s upstreamSpec) string {
	if strings.TrimSpace(s.Token) != "" {
		return strings.TrimSpace(s.Token)
	}
	if s.TokenEnv != "" {
		if v := os.Getenv(s.TokenEnv); v != "" {
			return v
		}
	}
	if v := os.Getenv("HUB_UPSTREAM_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("API_TOKEN")
}

func (h *mergedHub) passFor(s upstreamSpec) string {
	if strings.TrimSpace(s.SocksPass) != "" {
		return strings.TrimSpace(s.SocksPass)
	}
	if s.SocksPassEnv != "" {
		if v := os.Getenv(s.SocksPassEnv); v != "" {
			return v
		}
	}
	if v := os.Getenv("UPSTREAM_SOCKS_PASS"); v != "" {
		return v
	}
	if h.passLookup != nil {
		return h.passLookup(s)
	}
	return ""
}

func (h *mergedHub) userFor(s upstreamSpec) string {
	if strings.TrimSpace(s.SocksUser) != "" {
		return strings.TrimSpace(s.SocksUser)
	}
	return h.defaultUser
}

func (h *mergedHub) fetchStatus(s upstreamSpec) (*upstreamStatusDoc, error) {
	req, err := http.NewRequest(http.MethodGet, s.BaseURL+"/api/v1/status", nil)
	if err != nil {
		return nil, err
	}
	if tok := h.tokenFor(s); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, snippet(body, 120))
	}
	var doc upstreamStatusDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &doc, nil
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (h *mergedHub) snapshot(force bool) (*mergedSnapshot, error) {
	h.mu.Lock()
	if !force && h.cache != nil && time.Since(h.cacheAt) < h.ttl {
		c := h.cache
		h.mu.Unlock()
		return c, nil
	}
	h.mu.Unlock()

	specs, err := h.loadSpecs()
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no enabled upstreams in %s", h.path)
	}

	type fetchResult struct {
		spec upstreamSpec
		view mergedUpstreamView
		doc  *upstreamStatusDoc
	}
	results := make([]fetchResult, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func(i int, s upstreamSpec) {
			defer wg.Done()
			start := time.Now()
			view := mergedUpstreamView{Name: s.Name, BaseURL: s.BaseURL}
			doc, err := h.fetchStatus(s)
			view.LatencyMS = time.Since(start).Milliseconds()
			if err != nil {
				view.Error = err.Error()
			} else {
				view.OK = true
				view.Nodes = len(doc.Nodes)
			}
			results[i] = fetchResult{spec: s, view: view, doc: doc}
		}(i, s)
	}
	wg.Wait()

	type candidate struct {
		key  string
		node mergedNode
	}
	best := make(map[string]candidate)
	views := make([]mergedUpstreamView, 0, len(results))
	for _, r := range results {
		if r.doc == nil {
			views = append(views, r.view)
			continue
		}
		seen := make(map[string]bool)
		ok := 0
		for _, n := range r.doc.Nodes {
			prefix := strings.TrimSpace(n.Prefix64)
			if !n.Enabled || n.State != "ok" || prefix == "" || n.User == "" {
				continue
			}
			ok++
			seen[prefix] = true
			host := strings.TrimSpace(r.spec.OverrideHost)
			if host == "" {
				if r.spec.scope() == "public" {
					host = strings.TrimSpace(r.doc.Pool.PublicHost)
				} else {
					host = strings.TrimSpace(r.doc.Pool.PrivateHost)
				}
			}
			if host == "" {
				host = hostFromURL(r.spec.BaseURL)
			}
			port := r.spec.OverridePort
			if port <= 0 {
				port = r.doc.Pool.MuxPort
			}
			if port <= 0 {
				port = n.Port
			}
			if host == "" || port <= 0 {
				continue
			}
			node := mergedNode{
				Prefix64: prefix,
				Upstream: r.spec.Name,
				User:     n.User,
				Host:     host,
				Port:     port,
				Loc:      n.Loc,
				URL:      fmt.Sprintf("socks5://%s:%s@%s:%d", n.User, h.passFor(r.spec), host, port),
			}
			key := r.spec.Name + "\x00" + n.User
			if prev, exists := best[prefix]; !exists || key < prev.key {
				best[prefix] = candidate{key: key, node: node}
			}
		}
		r.view.NodesOK = ok
		r.view.Unique64 = len(seen)
		views = append(views, r.view)
	}

	nodes := make([]mergedNode, 0, len(best))
	for _, c := range best {
		nodes = append(nodes, c.node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Prefix64 < nodes[j].Prefix64 })

	snap := &mergedSnapshot{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Unique64:    len(nodes),
		Endpoints:   len(nodes),
		Upstreams:   views,
		Nodes:       nodes,
	}

	h.mu.Lock()
	h.cache = snap
	h.cacheAt = time.Now()
	h.mu.Unlock()
	return snap, nil
}

func (a *apiServer) handleMergedStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	snap, err := a.hub.snapshot(r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *apiServer) handleMergedEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	snap, err := a.hub.snapshot(r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	lines := make([]string, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		if n.URL != "" {
			lines = append(lines, n.URL)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Merged-Unique64", strconv.Itoa(snap.Unique64))
	w.Header().Set("X-Merged-Endpoints", strconv.Itoa(len(lines)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}
