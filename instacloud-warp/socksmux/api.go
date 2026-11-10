package main

import (
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
	StateDir      string
	Token         string
	PoolID        string
	PublicHost    string
	PrivateHost   string
	MuxPort       int
	SocksUser     string
	Rotation      int
	Batch         int
	Health        int
	MinHealthy    int
	UpstreamsFile string
	RelaySlots    int
	RelayStateDir string
}

type apiServer struct {
	m           *mux
	stateDir    string
	token       string
	tokens      *tokenStore
	adminUser   string
	adminPass   string
	sessionKey  []byte
	poolID      string
	publicHost  string
	privateHost string
	muxPort     int
	socksUser   string
	rotation    int
	batch       int
	health      int
	minHealthy  int
	relaySlots  int
	relayStateDir string
	hub         *mergedHub
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
	Reserved    bool   `json:"reserved,omitempty"`
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
	ID                      string `json:"id"`
	PublicHost              string `json:"public_host,omitempty"`
	PrivateHost             string `json:"private_host,omitempty"`
	MuxPort                 int    `json:"mux_port"`
	SocksUser               string `json:"socks_user"`
	Count                   int    `json:"count"`
	Healthy                 int    `json:"healthy"`
	Enabled                 int    `json:"enabled"`
	RotationIntervalSeconds int    `json:"rotation_interval_seconds"`
	RerollBatch             int    `json:"reroll_batch"`
	HealthIntervalSeconds   int    `json:"health_interval_seconds"`
	MinHealthy              int    `json:"min_healthy"`
	UpdatedAt               string `json:"updated_at"`
}

type statusResponse struct {
	Pool  poolView   `json:"pool"`
	Nodes []nodeView `json:"nodes"`
}

type metaAPI struct {
	PrivateURL string `json:"private_url"`
	PublicURL  string `json:"public_url"`
	Token      string `json:"token"`
}

type metaSocks struct {
	User    string `json:"user"`
	Pass    string `json:"pass"`
	MuxPort int    `json:"mux_port"`
}

type metaDeploy struct {
	StateDir         string `json:"state_dir"`
	StartPort        int    `json:"start_port"`
	InstanceCount    int    `json:"instance_count"`
	InstanceCountEnv int    `json:"instance_count_env"`
	MuxPort          int    `json:"mux_port"`
	MuxListen        string `json:"mux_listen"`
	RerollInterval   int    `json:"reroll_interval"`
	RerollBatch      int    `json:"reroll_batch"`
	HealthInterval   int    `json:"health_interval"`
	MinHealthy       int    `json:"min_healthy"`
	DedupRetries     int    `json:"dedup_retries"`
	RegisterRetries  int    `json:"register_retries"`
	ProbeURL         string `json:"probe_url"`
	ProbeTimeout     int    `json:"probe_timeout"`
	EndpointsFile    string `json:"endpoints_file"`
	EndpointsCount   int    `json:"endpoints_count"`
	Deny48Prefixes   string `json:"deny_48_prefixes"`
	PoolPublicHost   string `json:"pool_public_host"`
	PoolPrivateHost  string `json:"pool_private_host"`
	ExternalHost     string `json:"external_host"`
	Keepalive        int    `json:"keepalive"`
	PortStart        int    `json:"port_start"`
	PortEnd          int    `json:"port_end"`
	ScaleMin         int    `json:"scale_min"`
	ScaleMax         int    `json:"scale_max"`
	AdminUser        string `json:"admin_user"`
	RelaySlots       int    `json:"relay_slots"`
}

type metaEndpoints struct {
	File  string   `json:"file"`
	Count int      `json:"count"`
	Items []string `json:"items"`
}

type metaRegion struct {
	Loc      string   `json:"loc"`
	Count    int      `json:"count"`
	Healthy  int      `json:"healthy"`
	Prefixes []string `json:"prefixes"`
}

type metaResponse struct {
	Pool      poolView      `json:"pool"`
	API       metaAPI       `json:"api"`
	Socks     metaSocks     `json:"socks"`
	Deploy    metaDeploy    `json:"deploy"`
	Endpoints metaEndpoints `json:"endpoints"`
	Regions   []metaRegion  `json:"regions"`
	Runtime   runtimeConfig `json:"runtime"`
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
	adminUser := envString("POOL_ADMIN_USER", "admin")
	adminPass := envString("POOL_ADMIN_PASSWORD", "WarpPass2026")
	return &apiServer{
		m:           m,
		stateDir:    o.StateDir,
		token:       o.Token,
		tokens:      newTokenStore(o.StateDir, o.Token),
		adminUser:   adminUser,
		adminPass:   adminPass,
		sessionKey:  sessionKey(adminUser, adminPass, o.Token),
		poolID:      o.PoolID,
		publicHost:  o.PublicHost,
		privateHost: o.PrivateHost,
		muxPort:     o.MuxPort,
		socksUser:   user,
		rotation:    o.Rotation,
		batch:       o.Batch,
		health:      o.Health,
		minHealthy:  o.MinHealthy,
		relaySlots:  o.RelaySlots,
		relayStateDir: o.RelayStateDir,
		hub:         newMergedHub(o.StateDir, o.UpstreamsFile, user, func(upstreamSpec) string { return m.clientPass }),
		ln:          newConnListener(nil),
	}
}

func (a *apiServer) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleUI)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/api/v1/login", a.handleLogin)
	mux.HandleFunc("/api/v1/logout", a.handleLogout)
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
		if a.token == "" {
			next(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.Header.Get("X-API-Token")
		}
		if a.tokens.matches(tok) || a.validSession(r) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, "unauthorized")
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
	case path == "/api/v1/meta":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleMeta(w, r)
	case path == "/api/v1/scale":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleScale(w, r)
	case path == "/api/v1/endpoints":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleEndpoints(w, r)
	case path == "/api/v1/tokens":
		a.handleTokens(w, r)
	case strings.HasPrefix(path, "/api/v1/tokens/"):
		a.handleTokenItem(w, r, strings.TrimPrefix(path, "/api/v1/tokens/"))
	case path == "/api/v1/relay":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleRelay(w, r)
	case path == "/api/v1/merged-status":
		a.handleMergedStatus(w, r)
	case path == "/api/v1/merged-endpoints":
		a.handleMergedEndpoints(w, r)
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

func envInt(key string, fallback int) int {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// handleMeta exposes the connection material and deployment knobs the pool
// console needs: API origins, token, SOCKS credentials, env defaults, the
// anycast endpoint file and the regions currently seen on the nodes.
func (a *apiServer) handleMeta(w http.ResponseWriter, r *http.Request) {
	st := a.status()
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	publicURL := scheme + "://" + r.Host
	privateURL := ""
	if a.privateHost != "" {
		privateURL = "http://" + a.privateHost
		if a.muxPort > 0 && a.muxPort != 80 {
			privateURL += ":" + strconv.Itoa(a.muxPort)
		}
	}
	endpointsFile := envString("ENDPOINTS_FILE", "/usr/local/share/warp-pool/endpoints.txt")
	endpoints := make([]string, 0, 32)
	if data, err := os.ReadFile(endpointsFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			endpoints = append(endpoints, line)
			if len(endpoints) >= 1024 {
				break
			}
		}
	}
	regions := make([]metaRegion, 0)
	regionIndex := map[string]int{}
	for _, node := range st.Nodes {
		loc := strings.TrimSpace(node.Loc)
		if node.Reserved {
			loc = "-"
		} else if loc == "" {
			loc = "unknown"
		}
		pos, ok := regionIndex[loc]
		if !ok {
			pos = len(regions)
			regionIndex[loc] = pos
			regions = append(regions, metaRegion{Loc: loc})
		}
		regions[pos].Count++
		if node.Healthy {
			regions[pos].Healthy++
		}
		prefix := strings.TrimSpace(node.Prefix64)
		if prefix != "" && len(regions[pos].Prefixes) < 8 {
			found := false
			for _, existing := range regions[pos].Prefixes {
				if existing == prefix {
					found = true
					break
				}
			}
			if !found {
				regions[pos].Prefixes = append(regions[pos].Prefixes, prefix)
			}
		}
	}
	sort.Slice(regions, func(i, j int) bool {
		if regions[i].Count != regions[j].Count {
			return regions[i].Count > regions[j].Count
		}
		return regions[i].Loc < regions[j].Loc
	})
	scaleMax := len(endpoints)
	if scaleMax == 0 || scaleMax > 1024 {
		scaleMax = 1024
	}
	deploy := metaDeploy{
		StateDir:         envString("STATE_DIR", a.stateDir),
		StartPort:        a.m.basePort,
		InstanceCount:    a.m.count,
		InstanceCountEnv: envInt("INSTANCE_COUNT", a.m.count),
		MuxPort:          a.muxPort,
		MuxListen:        envString("MUX_LISTEN", "0.0.0.0"),
		RerollInterval:   envInt("REROLL_INTERVAL", a.rotation),
		RerollBatch:      envInt("REROLL_BATCH", a.batch),
		HealthInterval:   envInt("HEALTH_INTERVAL", a.health),
		MinHealthy:       envInt("MIN_HEALTHY", a.minHealthy),
		DedupRetries:     envInt("DEDUP_RETRIES", 4),
		RegisterRetries:  envInt("REGISTER_RETRIES", 3),
		ProbeURL:         envString("PROBE_URL", "https://opencode.ai/cdn-cgi/trace"),
		ProbeTimeout:     envInt("PROBE_TIMEOUT", 6),
		EndpointsFile:    endpointsFile,
		EndpointsCount:   len(endpoints),
		Deny48Prefixes:   envString("DENY_48_PREFIXES", ""),
		PoolPublicHost:   envString("POOL_PUBLIC_HOST", a.publicHost),
		PoolPrivateHost:  envString("POOL_PRIVATE_HOST", a.privateHost),
		ExternalHost:     envString("EXTERNAL_HOST", ""),
		Keepalive:        envInt("WGCF_KEEPALIVE", 25),
		PortStart:        a.m.basePort,
		PortEnd:          a.m.basePort + a.m.count - 1,
		ScaleMin:         1,
		ScaleMax:         scaleMax,
		AdminUser:        a.adminUser,
		RelaySlots:       a.relaySlots,
	}
	writeJSON(w, http.StatusOK, metaResponse{
		Pool:      st.Pool,
		API:       metaAPI{PrivateURL: privateURL, PublicURL: publicURL, Token: a.token},
		Socks:     metaSocks{User: a.socksUser, Pass: a.m.clientPass, MuxPort: a.muxPort},
		Deploy:    deploy,
		Endpoints: metaEndpoints{File: endpointsFile, Count: len(endpoints), Items: endpoints},
		Regions:   regions,
		Runtime:   a.config(),
	})
}

// handleScale queues a runtime node-count change. The entrypoint starts or
// stops wireproxy instances and restarts the mux with the new fan-out count.
func (a *apiServer) handleScale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Count < 1 || body.Count > 1024 {
		writeErr(w, http.StatusBadRequest, "count must be between 1 and 1024")
		return
	}
	if a.muxPort > 0 && a.m.basePort <= a.muxPort && a.muxPort <= a.m.basePort+body.Count-1 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("mux port %d conflicts with node ports %d-%d", a.muxPort, a.m.basePort, a.m.basePort+body.Count-1))
		return
	}
	if err := a.queue(fmt.Sprintf("scale %d", body.Count)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "count": body.Count})
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
		reserved := a.relaySlots > 0 && i >= a.m.count-a.relaySlots
		if !reserved {
			if isHealthy {
				healthy++
			}
			if isEnabled {
				enabled++
			}
		}
		n := nodeView{
			User:      user,
			Index:     i + 1,
			Port:      port,
			State:     state,
			Healthy:   isHealthy,
			Enabled:   isEnabled,
			Reserved:  reserved,
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
			ID:                      a.poolID,
			PublicHost:              a.publicHost,
			PrivateHost:             a.privateHost,
			MuxPort:                 a.muxPort,
			SocksUser:               a.socksUser,
			Count:                   a.m.count,
			Healthy:                 healthy,
			Enabled:                 enabled,
			RotationIntervalSeconds: cfg.RotationIntervalSeconds,
			RerollBatch:             cfg.RerollBatch,
			HealthIntervalSeconds:   cfg.HealthIntervalSeconds,
			MinHealthy:              cfg.MinHealthy,
			UpdatedAt:               time.Now().UTC().Format(time.RFC3339),
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
		if n.Reserved || !n.Enabled || n.State != "ok" {
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
	if envString("RELAY_LENIENT_HEALTH", "") == "1" && st.Pool.Count > 0 {
		ok = true
	}
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
