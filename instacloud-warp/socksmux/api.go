package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiOptions struct {
	StateDir    string
	Token       string
	PoolID      string
	PublicHost  string
	PrivateHost string
	MuxPort     int
	SocksUser   string
	Rotation    int
	Batch       int
	Health      int
	MinHealthy  int
}

type apiServer struct {
	m           *mux
	stateDir    string
	token       string
	poolID      string
	publicHost  string
	privateHost string
	muxPort     int
	socksUser   string
	rotation    int
	batch       int
	health      int
	minHealthy  int
	ln          *connListener
}

type nodeStatus struct {
	Port     int
	PID      int
	Endpoint string
	Prefix   string
	Loc      string
	State    string
	Updated  int64
}

type nodeView struct {
	User        string `json:"user"`
	Index       int    `json:"index"`
	Port        int    `json:"port"`
	State       string `json:"state"`
	Healthy     bool   `json:"healthy"`
	Enabled     bool   `json:"enabled"`
	ExitIPv6    string `json:"exit_ipv6,omitempty"`
	Prefix64    string `json:"prefix64,omitempty"`
	Loc         string `json:"loc,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	PID         int    `json:"pid,omitempty"`
	UpdatedAt   int64  `json:"updated_at,omitempty"`
	Connections uint64 `json:"connections"`
	BytesUp     uint64 `json:"bytes_up"`
	BytesDown   uint64 `json:"bytes_down"`
}

type poolView struct {
	ID                     string `json:"id"`
	PublicHost             string `json:"public_host,omitempty"`
	PrivateHost            string `json:"private_host,omitempty"`
	MuxPort                int    `json:"mux_port"`
	SocksUser              string `json:"socks_user"`
	Count                  int    `json:"count"`
	Healthy                int    `json:"healthy"`
	Enabled                int    `json:"enabled"`
	RotationIntervalSeconds int    `json:"rotation_interval_seconds"`
	RerollBatch            int    `json:"reroll_batch"`
	HealthIntervalSeconds  int    `json:"health_interval_seconds"`
	MinHealthy             int    `json:"min_healthy"`
	UpdatedAt              string `json:"updated_at"`
}

type statusResponse struct {
	Pool  poolView   `json:"pool"`
	Nodes []nodeView `json:"nodes"`
}

type runtimeConfig struct {
	RotationIntervalSeconds int `json:"rotation_interval_seconds"`
	RerollBatch             int `json:"reroll_batch"`
	HealthIntervalSeconds   int `json:"health_interval_seconds"`
	MinHealthy              int `json:"min_healthy"`
}

type connListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{
		ch:     make(chan net.Conn, 128),
		closed: make(chan struct{}),
		addr:   addr,
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr { return l.addr }

func (l *connListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	default:
		_ = c.Close()
		return false
	}
}

func newAPIServer(m *mux, o apiOptions) *apiServer {
	user := o.SocksUser
	if user == "" {
		user = m.upUser
	}
	return &apiServer{
		m:           m,
		stateDir:    o.StateDir,
		token:       o.Token,
		poolID:      o.PoolID,
		publicHost:  o.PublicHost,
		privateHost: o.PrivateHost,
		muxPort:     o.MuxPort,
		socksUser:   user,
		rotation:    o.Rotation,
		batch:       o.Batch,
		health:      o.Health,
		minHealthy:  o.MinHealthy,
		ln:          newConnListener(nil),
	}
}

func (a *apiServer) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleUI)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/api/v1/", a.auth(a.handleAPI))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.Serve(a.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("api: %v", err)
	}
}

func (a *apiServer) dispatch(c net.Conn) {
	if !a.ln.push(c) {
		_ = c.Close()
	}
}

func (a *apiServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.Header.Get("X-API-Token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (a *apiServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/api/v1/status":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleStatus(w, r)
	case path == "/api/v1/endpoints":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleEndpoints(w, r)
	case path == "/api/v1/config":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, a.config())
		case http.MethodPut, http.MethodPost:
			a.handleConfigUpdate(w, r)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case path == "/api/v1/reroll":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := a.queue("reroll-all"); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "action": "reroll-all"})
	case path == "/api/v1/logs":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleLogs(w, r)
	case strings.HasPrefix(path, "/api/v1/nodes/"):
		a.handleNode(w, r, path)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (a *apiServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.status())
}

func (a *apiServer) status() statusResponse {
	cfg := a.config()
	now := time.Now().Unix()
	fresh := int64(cfg.HealthIntervalSeconds * 3)
	if fresh < 900 {
		fresh = 900
	}
	status := readStatus(a.stateDir)
	disabled := readDisabled(a.stateDir)
	nodes := make([]nodeView, 0, a.m.count)
	healthy, enabled := 0, 0
	for i := 0; i < a.m.count; i++ {
		port := a.m.basePort + i
		user := fmt.Sprintf("%s%02d", a.socksUser, i+1)
		st, ok := status[port]
		state := "unknown"
		if ok {
			state = st.State
		}
		isEnabled := !disabled[user]
		isHealthy := isEnabled && state == "ok" && now-st.Updated <= fresh
		if isHealthy {
			healthy++
		}
		if isEnabled {
			enabled++
		}
		n := nodeView{
			User:      user,
			Index:     i + 1,
			Port:      port,
			State:     state,
			Healthy:   isHealthy,
			Enabled:   isEnabled,
			Prefix64:  st.Prefix,
			Loc:       st.Loc,
			Endpoint:  st.Endpoint,
			PID:       st.PID,
			UpdatedAt: st.Updated,
		}
		if st.Prefix != "" && st.Prefix != "-" {
			n.ExitIPv6 = st.Prefix
		}
		if i < len(a.m.stats) {
			n.Connections = a.m.stats[i].conns.Load()
			n.BytesUp = a.m.stats[i].up.Load()
			n.BytesDown = a.m.stats[i].down.Load()
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Index < nodes[j].Index })
	return statusResponse{
		Pool: poolView{
			ID:                     a.poolID,
			PublicHost:             a.publicHost,
			PrivateHost:            a.privateHost,
			MuxPort:                a.muxPort,
			SocksUser:              a.socksUser,
			Count:                  a.m.count,
			Healthy:                healthy,
			Enabled:                enabled,
			RotationIntervalSeconds: cfg.RotationIntervalSeconds,
			RerollBatch:            cfg.RerollBatch,
			HealthIntervalSeconds:  cfg.HealthIntervalSeconds,
			MinHealthy:             cfg.MinHealthy,
			UpdatedAt:              time.Now().UTC().Format(time.RFC3339),
		},
		Nodes: nodes,
	}
}

func (a *apiServer) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "private"
	}
	var host string
	switch scope {
	case "private":
		host = a.privateHost
	case "public":
		host = a.publicHost
	default:
		writeErr(w, http.StatusBadRequest, "scope must be private or public")
		return
	}
	if host == "" {
		writeErr(w, http.StatusConflict, "host for scope "+scope+" is not configured")
		return
	}
	st := a.status()
	type endpoint struct {
		User string `json:"user"`
		URL  string `json:"url"`
	}
	endpoints := make([]endpoint, 0, len(st.Nodes))
	lines := make([]string, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		if !n.Enabled || n.State != "ok" {
			continue
		}
		port := a.muxPort
		if port <= 0 {
			port = n.Port
		}
		u := fmt.Sprintf("socks5://%s:%s@%s:%d", n.User, a.m.clientPass, host, port)
		endpoints = append(endpoints, endpoint{User: n.User, URL: u})
		lines = append(lines, u)
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, map[string]any{
			"scope":     scope,
			"host":      host,
			"port":      a.muxPort,
			"count":     len(lines),
			"endpoints": endpoints,
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Pool-Healthy", strconv.Itoa(st.Pool.Healthy))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

func (a *apiServer) handleConfigUpdate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var patch runtimeConfig
	if err := json.Unmarshal(body, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	cfg := a.config()
	if patch.RotationIntervalSeconds != 0 {
		cfg.RotationIntervalSeconds = patch.RotationIntervalSeconds
	}
	if patch.RerollBatch != 0 {
		cfg.RerollBatch = patch.RerollBatch
	}
	if patch.HealthIntervalSeconds != 0 {
		cfg.HealthIntervalSeconds = patch.HealthIntervalSeconds
	}
	if _, ok := probe["min_healthy"]; ok {
		cfg.MinHealthy = patch.MinHealthy
	}
	if cfg.RotationIntervalSeconds < 60 || cfg.RotationIntervalSeconds > 86400 {
		writeErr(w, http.StatusBadRequest, "rotation_interval_seconds must be 60..86400")
		return
	}
	if cfg.RerollBatch < 1 || cfg.RerollBatch > a.m.count {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("reroll_batch must be 1..%d", a.m.count))
		return
	}
	if cfg.HealthIntervalSeconds < 30 || cfg.HealthIntervalSeconds > 86400 {
		writeErr(w, http.StatusBadRequest, "health_interval_seconds must be 30..86400")
		return
	}
	if cfg.MinHealthy < 0 || cfg.MinHealthy > a.m.count {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("min_healthy must be 0..%d", a.m.count))
		return
	}
	dir := filepath.Join(a.stateDir, "control")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := writeFileAtomic(filepath.Join(dir, "config.json"), mustJSONValue(cfg), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.queue("reload-config"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, cfg)
}

func mustJSONValue(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

func (a *apiServer) handleNode(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(path, "/api/v1/nodes/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	user, action := parts[0], parts[1]
	if _, ok := a.userIndex(user); !ok {
		writeErr(w, http.StatusNotFound, "unknown node "+user)
		return
	}
	switch action {
	case "rotate", "enable", "disable":
	default:
		writeErr(w, http.StatusNotFound, "unknown action "+action)
		return
	}
	if err := a.queue(action + " " + user); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "user": user, "action": action})
}

func (a *apiServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 2000 {
			n = parsed
		}
	}
	b, err := os.ReadFile(filepath.Join(a.stateDir, "socksmux.log"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "log not available")
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

func (a *apiServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	st := a.status()
	ok := st.Pool.Healthy >= st.Pool.MinHealthy && st.Pool.Healthy > 0
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":  map[bool]string{true: "ok", false: "degraded"}[ok],
		"healthy": st.Pool.Healthy,
		"enabled": st.Pool.Enabled,
		"count":   st.Pool.Count,
	})
}

func (a *apiServer) config() runtimeConfig {
	cfg := runtimeConfig{
		RotationIntervalSeconds: a.rotation,
		RerollBatch:             a.batch,
		HealthIntervalSeconds:   a.health,
		MinHealthy:              a.minHealthy,
	}
	b, err := os.ReadFile(filepath.Join(a.stateDir, "control", "config.json"))
	if err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	return cfg
}

func (a *apiServer) queue(cmd string) error {
	dir := filepath.Join(a.stateDir, "control", "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cmd-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.WriteString(cmd + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	final := filepath.Join(dir, fmt.Sprintf("%020d.cmd", time.Now().UnixNano()))
	return os.Rename(name, final)
}

func (a *apiServer) userIndex(user string) (int, bool) {
	if !strings.HasPrefix(user, a.socksUser) {
		return 0, false
	}
	suffix := strings.TrimPrefix(user, a.socksUser)
	if suffix == "" {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || n > a.m.count {
		return 0, false
	}
	return n - 1, true
}

func readStatus(dir string) map[int]nodeStatus {
	out := make(map[int]nodeStatus)
	b, err := os.ReadFile(filepath.Join(dir, "status.tsv"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		port, _ := strconv.Atoi(f[0])
		pid, _ := strconv.Atoi(f[1])
		ts, _ := strconv.ParseInt(f[6], 10, 64)
		if port == 0 {
			continue
		}
		out[port] = nodeStatus{
			Port:     port,
			PID:      pid,
			Endpoint: f[2],
			Prefix:   f[3],
			Loc:      f[4],
			State:    f[5],
			Updated:  ts,
		}
	}
	return out
}

func readDisabled(dir string) map[string]bool {
	out := make(map[string]bool)
	b, err := os.ReadFile(filepath.Join(dir, "control", "disabled"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out[line] = true
		}
	}
	return out
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
