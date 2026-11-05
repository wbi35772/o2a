package admin

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/httpx"
)

// ProxyStore is the persisted state of the proxy center: the subscription
// providers, the cached node manifest with its last test results, and the
// operator settings. It lives in <state dir>/proxycenter/state.json.
type ProxyStore struct {
	Providers []ProxyProvider `json:"providers"`
	CFSources []CFSource      `json:"cf_sources,omitempty"`
	Nodes     []ProxyNode     `json:"nodes"`
	Settings  ProxySettings   `json:"settings"`
	SyncedAt  string          `json:"synced_at,omitempty"`
	SyncError string          `json:"last_sync_error,omitempty"`
}

// ProxyProvider is one subscription source: an http(s) Clash/plain
// subscription URL, or a local Clash YAML path (imported or manual).
type ProxyProvider struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	NodeCount int    `json:"node_count"`
	LastFetch string `json:"last_fetch,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// ProxyNode mirrors one listener written by the sidecar. Name is the mihomo
// proxy name and the stable identity across syncs.
type ProxyNode struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	Server     string `json:"server"`
	Port       int    `json:"port"`
	ListenPort int    `json:"listen_port"`
	Status     string `json:"status"`
	LatencyMs  int64  `json:"latency_ms"`
	LastTest   string `json:"last_test,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	Selected   bool   `json:"selected"`
	// EgressIP and Quota are filled by the quota probe: which public address
	// this listener presents and whether that address still has free quota.
	EgressIP       string `json:"egress_ip,omitempty"`
	Quota          string `json:"quota,omitempty"`
	QuotaNote      string `json:"quota_note,omitempty"`
	QuotaCheckedAt string `json:"quota_checked_at,omitempty"`
	RetryAfter     int64  `json:"retry_after_seconds,omitempty"`
}

// ProxySettings controls how the sidecar samples nodes and how the tester
// classifies them.
type ProxySettings struct {
	NodeCount         int    `json:"node_count"`
	PortBase          int    `json:"port_base"`
	TestURL           string `json:"test_url"`
	TestTimeoutSec    int    `json:"test_timeout_seconds"`
	AutoSelectHealthy bool   `json:"auto_select_healthy"`
}

// ProxyJob reports the single background job the proxy center runs.
type ProxyJob struct {
	Running    bool   `json:"running"`
	Action     string `json:"action"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// proxyRuntime is the sidecar/app summary shown on the page.
type proxyRuntime struct {
	MihomoPID    int            `json:"mihomo_pid"`
	MihomoAlive  bool           `json:"mihomo_alive"`
	Listeners    int            `json:"listeners"`
	ProxyFile    int            `json:"proxylines"`
	Healthy      int            `json:"healthy"`
	Limited      int            `json:"limited"`
	Dead         int            `json:"dead"`
	Unknown      int            `json:"unknown"`
	Statuses     map[string]int `json:"statuses"`
	Selected     int            `json:"selected"`
	QuotaOK      int            `json:"quota_ok"`
	QuotaLimited int            `json:"quota_limited"`
	QuotaBlocked int            `json:"quota_blocked"`
	QuotaError   int            `json:"quota_error"`
	QuotaUnknown int            `json:"quota_unknown"`
	LastSync     string         `json:"last_sync,omitempty"`
	LastSyncFail string         `json:"last_sync_error,omitempty"`
}

type proxyListener struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	Server     string `json:"server"`
	Port       int    `json:"port"`
	ListenPort int    `json:"listen_port"`
}

func defaultProxySettings() ProxySettings {
	return ProxySettings{
		NodeCount:         600,
		PortBase:          21000,
		TestURL:           "https://opencode.ai/",
		TestTimeoutSec:    8,
		AutoSelectHealthy: false,
	}
}

// proxyDir returns the proxy center directory under the service state dir.
func (a *Server) proxyDir() string {
	return filepath.Join(a.stateDir, "proxycenter")
}

// proxyWorkDir is where the sidecar writes the fleet it generated
// (mihomo.json, proxies.txt, its log). proxyctl defaults it to
// <state>/cfnew and honours PROXY_WORK_DIR, so read the same value here
// instead of assuming the proxy-center directory.
func (a *Server) proxyWorkDir() string {
	if dir := strings.TrimSpace(os.Getenv("PROXY_WORK_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(a.stateDir, "cfnew")
}

func (a *Server) proxyStatePath() string { return filepath.Join(a.proxyDir(), "state.json") }

// loadProxyStore reads state.json, filling in defaults when absent. When the
// file is missing (a container recreated from a volume that only kept the
// sidecar output, or the first boot after an upgrade) the store is rebuilt
// from listeners.json and selected.txt so the panel shows the live fleet.
func (a *Server) loadProxyStore() ProxyStore {
	store := ProxyStore{Settings: defaultProxySettings()}
	data, err := os.ReadFile(a.proxyStatePath())
	if err != nil {
		return a.recoverProxyStore(store)
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return ProxyStore{Settings: defaultProxySettings()}
	}
	if store.Settings.NodeCount <= 0 {
		store.Settings.NodeCount = 600
	}
	if store.Settings.PortBase <= 0 {
		store.Settings.PortBase = 21000
	}
	if store.Settings.TestURL == "" {
		store.Settings.TestURL = "https://opencode.ai/"
	}
	if store.Settings.TestTimeoutSec <= 0 {
		store.Settings.TestTimeoutSec = 8
	}
	return store
}

// recoverProxyStore rebuilds the in-memory store from what the sidecar left on
// disk: listeners.json describes the current fleet, selected.txt the names the
// gateway actually uses, and cfnew-sub.txt the legacy subscription list.
func (a *Server) recoverProxyStore(store ProxyStore) ProxyStore {
	listeners := a.readListeners()
	if len(listeners) == 0 {
		return store
	}
	mergeListeners(&store, listeners, a.readSelected())
	if data, err := os.ReadFile(filepath.Join(a.stateDir, "cfnew-sub.txt")); err == nil {
		index := 0
		for _, line := range strings.Split(string(data), "\n") {
			url := strings.TrimSpace(line)
			if url == "" || strings.HasPrefix(url, "#") {
				continue
			}
			index++
			store.Providers = append(store.Providers, ProxyProvider{
				ID: fmt.Sprintf("p%d", index), Name: fmt.Sprintf("subscription %d", index),
				URL: url, Enabled: true, LastFetch: store.SyncedAt,
			})
		}
	}
	return store
}

// saveProxyStore writes state.json atomically.
func (a *Server) saveProxyStore(store ProxyStore) error {
	dir := a.proxyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.proxyStatePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.proxyStatePath())
}

// writePrivateFile writes a proxy-center file with owner-only permissions,
// tightening a file that already exists with a looser mode. Provider URLs can
// embed subscription tokens and the state lists proxy credentials.
func writePrivateFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// writeProvidersFile renders the enabled providers for the sidecar: one
// "id<TAB>name<TAB>url" line each.
func (a *Server) writeProvidersFile(store ProxyStore) error {
	var b strings.Builder
	for _, p := range store.Providers {
		if !p.Enabled {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", p.ID, p.Name, p.URL)
	}
	return writePrivateFile(filepath.Join(a.proxyDir(), "providers.txt"), []byte(b.String()))
}

// writeSettingsFile renders the sampling settings the sidecar reads.
func (a *Server) writeSettingsFile(store ProxyStore) error {
	text := fmt.Sprintf("CF_NODE_COUNT=%d\nCF_PORT_BASE=%d\n", store.Settings.NodeCount, store.Settings.PortBase)
	return writePrivateFile(filepath.Join(a.proxyDir(), "settings.env"), []byte(text))
}

// proxyctlPath locates the sidecar script.
func proxyctlPath() string {
	if p := os.Getenv("PROXYCTL"); p != "" {
		return p
	}
	for _, candidate := range []string{"/usr/local/bin/proxyctl", "proxyctl"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	return "proxyctl"
}

// readListeners parses listeners.json written by an apply/sync run.
func (a *Server) readListeners() []proxyListener {
	data, err := os.ReadFile(filepath.Join(a.proxyDir(), "listeners.json"))
	if err != nil {
		return nil
	}
	var items []proxyListener
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}
	return items
}

// readSelected parses selected.txt (one mihomo proxy name per line).
func (a *Server) readSelected() map[string]bool {
	names := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(a.proxyDir(), "selected.txt"))
	if err != nil {
		return names
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			names[line] = true
		}
	}
	return names
}

func (a *Server) writeSelected(names []string) error {
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return writePrivateFile(filepath.Join(a.proxyDir(), "selected.txt"), []byte(b.String()))
}

// mergeListeners folds the sidecar manifest into the stored node list,
// preserving test results for nodes that still exist.
func mergeListeners(store *ProxyStore, listeners []proxyListener, selected map[string]bool) {
	previous := make(map[string]ProxyNode, len(store.Nodes))
	for _, node := range store.Nodes {
		previous[node.Name] = node
	}
	nodes := make([]ProxyNode, 0, len(listeners))
	for _, item := range listeners {
		node := ProxyNode{
			Name: item.Name, Provider: item.Provider, Type: item.Type,
			Server: item.Server, Port: item.Port, ListenPort: item.ListenPort,
			Status: "unknown", Selected: selected[item.Name],
		}
		if old, ok := previous[item.Name]; ok {
			node.Status, node.LatencyMs = old.Status, old.LatencyMs
			node.LastTest, node.LastError = old.LastTest, old.LastError
			node.EgressIP, node.Quota = old.EgressIP, old.Quota
			node.QuotaNote, node.QuotaCheckedAt = old.QuotaNote, old.QuotaCheckedAt
			node.RetryAfter = old.RetryAfter
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	store.Nodes = nodes
}

func (a *Server) proxyRuntime(store ProxyStore) proxyRuntime {
	rt := proxyRuntime{LastSync: store.SyncedAt, LastSyncFail: store.SyncError, Statuses: map[string]int{}}
	if pidData, err := os.ReadFile(filepath.Join(a.proxyDir(), "mihomo.pid")); err == nil {
		rt.MihomoPID, _ = strconv.Atoi(strings.TrimSpace(string(pidData)))
	}
	if rt.MihomoPID > 0 {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(rt.MihomoPID))); err == nil {
			rt.MihomoAlive = true
		}
	}
	rt.Listeners = len(store.Nodes)
	// proxyctl writes the fleet to its work directory, not to proxycenter.
	proxyFile := filepath.Join(a.proxyWorkDir(), "proxies.txt")
	if _, err := os.Stat(proxyFile); err != nil {
		proxyFile = filepath.Join(a.proxyDir(), "proxies.txt")
	}
	if data, err := os.ReadFile(proxyFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				rt.ProxyFile++
			}
		}
	}
	for _, node := range store.Nodes {
		rt.Statuses[node.Status]++
		switch node.Status {
		case "alive":
			rt.Healthy++
		case "limited":
			rt.Limited++
		case "dead":
			rt.Dead++
		default:
			rt.Unknown++
		}
		if node.Selected {
			rt.Selected++
		}
		switch node.Quota {
		case "ok":
			rt.QuotaOK++
		case "limited":
			rt.QuotaLimited++
		case "blocked":
			rt.QuotaBlocked++
		case "error":
			rt.QuotaError++
		default:
			rt.QuotaUnknown++
		}
	}
	return rt
}

// handleProxyState returns everything but the node list.
func (a *Server) handleProxyState(w http.ResponseWriter, _ *http.Request) {
	a.proxyJobMu.Lock()
	job := a.proxyJob
	a.proxyJobMu.Unlock()
	a.proxyMu.RLock()
	store := a.loadProxyStore()
	a.proxyMu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"providers":  store.Providers,
		"cf_sources": store.CFSources,
		"settings":   store.Settings,
		"runtime":    a.proxyRuntime(store),
		"job":        job,
	})
}

// handleProxyNodes serves the paginated, filtered node list.
func (a *Server) handleProxyNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	provider := q.Get("provider")
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(q.Get("size"))
	if size < 1 || size > 200 {
		size = 50
	}
	a.proxyMu.RLock()
	store := a.loadProxyStore()
	a.proxyMu.RUnlock()
	filtered := make([]ProxyNode, 0, len(store.Nodes))
	for _, node := range store.Nodes {
		if status != "" && status != "all" && node.Status != status {
			continue
		}
		if provider != "" && provider != "all" && node.Provider != provider {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(node.Name+" "+node.Server), search) {
			continue
		}
		filtered = append(filtered, node)
	}
	total := len(filtered)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"nodes": filtered[start:end], "total": total, "page": page, "size": size,
	})
}

// proxyJob running guard helpers.
func (a *Server) beginProxyJob(action string) error {
	a.proxyJobMu.Lock()
	defer a.proxyJobMu.Unlock()
	if a.proxyJob.Running {
		return errors.New("已有代理任务在运行，请稍候")
	}
	a.proxyJob = ProxyJob{Running: true, Action: action, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	return nil
}

func (a *Server) finishProxyJob(out string, err error) {
	a.proxyJobMu.Lock()
	defer a.proxyJobMu.Unlock()
	a.proxyJob.Running = false
	a.proxyJob.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	a.proxyJob.Output = truncateText(out, 6000)
	if err != nil {
		a.proxyJob.Error = err.Error()
	}
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}

// runProxyctl executes the sidecar action and returns its combined output.
func (a *Server) runProxyctl(ctx context.Context, action string) (string, error) {
	cmd := exec.CommandContext(ctx, proxyctlPath(), action)
	cmd.Dir = a.proxyDir()
	cmd.Env = append(os.Environ(), "CF_STATE_DIR="+a.proxyDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// syncProxy runs a full subscription fetch/merge/apply cycle, then reloads the
// gateway so the new proxy list is live.
func (a *Server) syncProxy(ctx context.Context) (string, error) {
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	if err := os.MkdirAll(a.proxyDir(), 0o700); err != nil {
		a.proxyMu.Unlock()
		return "", err
	}
	if err := a.writeProvidersFile(store); err != nil {
		a.proxyMu.Unlock()
		return "", err
	}
	if err := a.writeSettingsFile(store); err != nil {
		a.proxyMu.Unlock()
		return "", err
	}
	a.proxyMu.Unlock()

	out, err := a.runProxyctl(ctx, "sync")

	a.proxyMu.Lock()
	store = a.loadProxyStore()
	store.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		store.SyncError = truncateText(strings.TrimSpace(out), 500)
	} else {
		store.SyncError = ""
		mergeListeners(&store, a.readListeners(), a.readSelected())
		for i := range store.Providers {
			store.Providers[i].LastError, store.Providers[i].LastFetch = "", store.SyncedAt
		}
	}
	_ = a.saveProxyStore(store)
	a.proxyMu.Unlock()
	if err != nil {
		return out, err
	}
	if _, err := a.manager.Reload(); err != nil {
		return out, fmt.Errorf("代理已生成，但网关重载失败: %w", err)
	}
	return out, nil
}

func (a *Server) handleProxySync(w http.ResponseWriter, _ *http.Request) {
	err := a.startProxySyncJob()
	if err != nil {
		writeAdminError(w, http.StatusConflict, "proxy_busy", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (a *Server) startProxySyncJob() error {
	return a.startProxyJob("sync", func(ctx context.Context) (string, error) { return a.syncProxy(ctx) })
}

// startProxyJob runs fn in the background under the job guard. The caller must
// hold proxyMu.
func (a *Server) startProxyJob(action string, fn func(context.Context) (string, error)) error {
	if err := a.beginProxyJob(action); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := fn(ctx)
		a.finishProxyJob(out, err)
	}()
	return nil
}

func (a *Server) handleProxyProviderAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.URL = strings.TrimSpace(body.URL)
	if body.Name == "" || len(body.Name) > 80 {
		writeAdminError(w, http.StatusBadRequest, "invalid_name", "订阅名称需为 1–80 个字符")
		return
	}
	if body.URL == "" || len(body.URL) > 2048 {
		writeAdminError(w, http.StatusBadRequest, "invalid_url", "订阅地址无效")
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "/") {
		writeAdminError(w, http.StatusBadRequest, "invalid_url", "订阅地址需为 http(s) 链接或本地文件路径")
		return
	}
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	store.Providers = append(store.Providers, ProxyProvider{ID: id, Name: body.Name, URL: body.URL, Enabled: true})
	err := a.saveProxyStore(store)
	a.proxyMu.Unlock()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *Server) handleProxyProviderUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name    *string `json:"name"`
		URL     *string `json:"url"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	found := false
	for i := range store.Providers {
		if store.Providers[i].ID != id {
			continue
		}
		found = true
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			store.Providers[i].Name = strings.TrimSpace(*body.Name)
		}
		if body.URL != nil && strings.TrimSpace(*body.URL) != "" {
			store.Providers[i].URL = strings.TrimSpace(*body.URL)
		}
		if body.Enabled != nil {
			store.Providers[i].Enabled = *body.Enabled
		}
	}
	if !found {
		writeAdminError(w, http.StatusNotFound, "not_found", "订阅不存在")
		return
	}
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (a *Server) handleProxyProviderDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	kept := store.Providers[:0]
	for _, item := range store.Providers {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	store.Providers = kept
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *Server) handleProxySettings(w http.ResponseWriter, r *http.Request) {
	var body ProxySettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	if body.NodeCount < 1 || body.NodeCount > 2000 {
		writeAdminError(w, http.StatusBadRequest, "invalid_count", "节点数量需在 1–2000 之间")
		return
	}
	if body.PortBase < 1024 || body.PortBase > 60000-body.NodeCount {
		writeAdminError(w, http.StatusBadRequest, "invalid_port_base", "端口起点无效")
		return
	}
	if body.TestURL == "" {
		body.TestURL = "https://opencode.ai/"
	}
	if _, err := url.ParseRequestURI(body.TestURL); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_test_url", "测试地址无效")
		return
	}
	if body.TestTimeoutSec < 1 || body.TestTimeoutSec > 60 {
		body.TestTimeoutSec = 8
	}
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	store.Settings = body
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settings": body})
}

// handleProxySelect persists the selected names and rebuilds the listeners,
// then reloads the gateway.
func (a *Server) handleProxySelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names []string `json:"names"`
		Auto  bool     `json:"auto"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	names := body.Names
	if body.Auto {
		names = names[:0]
		for _, node := range store.Nodes {
			if node.Status == "alive" {
				names = append(names, node.Name)
			}
		}
	}
	if len(names) == 0 {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "empty_selection", "选择为空")
		return
	}
	if len(names) > 2000 {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusBadRequest, "too_many", "最多选择 2000 个节点")
		return
	}
	if err := a.writeSelected(names); err != nil {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	selected := a.readSelected()
	for i := range store.Nodes {
		store.Nodes[i].Selected = selected[store.Nodes[i].Name]
	}
	_ = a.saveProxyStore(store)
	err := a.startProxyJob("apply", func(ctx context.Context) (string, error) {
		out, err := a.runProxyctl(ctx, "apply")
		if err != nil {
			return out, err
		}
		listeners := a.readListeners()
		a.proxyMu.Lock()
		current := a.loadProxyStore()
		mergeListeners(&current, listeners, a.readSelected())
		_ = a.saveProxyStore(current)
		a.proxyMu.Unlock()
		if _, err := a.manager.Reload(); err != nil {
			return out, fmt.Errorf("网关重载失败: %w", err)
		}
		return out, nil
	})
	a.proxyMu.Unlock()
	if err != nil {
		writeAdminError(w, http.StatusConflict, "proxy_busy", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"selected": len(names)})
}

// handleProxyTest probes the requested nodes through their SOCKS listeners.
func (a *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names    []string `json:"names"`
		Status   string   `json:"status"`
		Provider string   `json:"provider"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body)
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	targets := make([]ProxyNode, 0, len(store.Nodes))
	wanted := map[string]bool{}
	for _, name := range body.Names {
		wanted[name] = true
	}
	for _, node := range store.Nodes {
		if len(wanted) > 0 && !wanted[node.Name] {
			continue
		}
		if body.Status != "" && body.Status != "all" && node.Status != body.Status {
			continue
		}
		if body.Provider != "" && body.Provider != "all" && node.Provider != body.Provider {
			continue
		}
		if node.ListenPort <= 0 {
			continue
		}
		targets = append(targets, node)
	}
	settings := store.Settings
	err := a.startProxyJob("test", func(ctx context.Context) (string, error) {
		tester := &proxyTester{target: settings.TestURL, timeout: time.Duration(settings.TestTimeoutSec) * time.Second}
		results := tester.run(ctx, targets)
		a.proxyMu.Lock()
		current := a.loadProxyStore()
		byName := make(map[string]ProxyPanelResult, len(results))
		for _, item := range results {
			byName[item.Name] = item
		}
		for i := range current.Nodes {
			if item, ok := byName[current.Nodes[i].Name]; ok {
				current.Nodes[i].Status = item.Status
				current.Nodes[i].LatencyMs = item.LatencyMs
				current.Nodes[i].LastError = item.Error
				current.Nodes[i].LastTest = time.Now().UTC().Format(time.RFC3339)
			}
		}
		if settings.AutoSelectHealthy {
			names := make([]string, 0, len(results))
			for _, item := range results {
				if item.Status == "alive" {
					names = append(names, item.Name)
				}
			}
			if len(names) > 0 {
				_ = a.writeSelected(names)
				for i := range current.Nodes {
					current.Nodes[i].Selected = current.Nodes[i].Status == "alive"
				}
				_ = a.saveProxyStore(current)
				a.proxyMu.Unlock()
				out, applyErr := a.runProxyctl(ctx, "apply")
				if applyErr != nil {
					return fmt.Sprintf("测试完成，自动应用失败: %s %s", out, applyErr), nil
				}
				if _, reloadErr := a.manager.Reload(); reloadErr != nil {
					return "测试完成，网关重载失败: " + reloadErr.Error(), nil
				}
				return fmt.Sprintf("测试 %d 个节点，可用 %d 个，已自动应用", len(results), len(names)), nil
			}
		}
		_ = a.saveProxyStore(current)
		a.proxyMu.Unlock()
		healthy := 0
		for _, item := range results {
			if item.Status == "alive" {
				healthy++
			}
		}
		return fmt.Sprintf("测试 %d 个节点，可用 %d 个", len(results), healthy), nil
	})
	a.proxyMu.Unlock()
	if err != nil {
		writeAdminError(w, http.StatusConflict, "proxy_busy", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"started": true, "targets": len(targets)})
}

// ProxyPanelResult is one node test outcome.
type ProxyPanelResult struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// proxyTester runs HTTPS probes through the local SOCKS listeners.
type proxyTester struct {
	target  string
	timeout time.Duration
}

func (t *proxyTester) run(ctx context.Context, nodes []ProxyNode) []ProxyPanelResult {
	results := make([]ProxyPanelResult, len(nodes))
	workers := 32
	if workers > len(nodes) {
		workers = len(nodes)
	}
	if workers < 1 {
		return results
	}
	indexes := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range indexes {
				node := nodes[idx]
				status, latency, errText := t.probe(ctx, node.ListenPort)
				results[idx] = ProxyPanelResult{Name: node.Name, Status: status, LatencyMs: latency, Error: errText}
			}
		}()
	}
	for i := range nodes {
		select {
		case <-ctx.Done():
			close(indexes)
			wg.Wait()
			return results
		case indexes <- i:
		}
	}
	close(indexes)
	wg.Wait()
	return results
}

// probe opens a SOCKS5 connection to the listener, performs a TLS GET against
// the test URL over it and classifies the outcome.
func (t *proxyTester) probe(ctx context.Context, port int) (status string, latencyMs int64, errText string) {
	parsed, err := url.Parse(t.target)
	if err != nil {
		return "dead", 0, "测试地址无效"
	}
	host := parsed.Hostname()
	if host == "" {
		return "dead", 0, "测试地址无效"
	}
	portNum := 443
	if parsed.Port() != "" {
		portNum, _ = strconv.Atoi(parsed.Port())
	} else if parsed.Scheme == "http" {
		portNum = 80
	}
	started := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	conn, err := socks5Dial(dialCtx, fmt.Sprintf("127.0.0.1:%d", port), host, portNum)
	if err != nil {
		return "dead", 0, truncateText(err.Error(), 160)
	}
	defer conn.Close()
	deadline := time.Now().Add(t.timeout)
	_ = conn.SetDeadline(deadline)
	var rw net.Conn = conn
	if parsed.Scheme != "http" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return "dead", 0, truncateText(err.Error(), 160)
		}
		rw = tlsConn
		tlsConn.SetDeadline(deadline)
	}
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: proxycenter/1\r\nAccept: */*\r\nConnection: close\r\n\r\n", path, host)
	if _, err := io.WriteString(rw, request); err != nil {
		return "dead", 0, truncateText(err.Error(), 160)
	}
	line, err := bufio.NewReader(rw).ReadString('\n')
	if err != nil {
		return "dead", 0, truncateText(err.Error(), 160)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "dead", 0, "响应格式无效"
	}
	code, _ := strconv.Atoi(fields[1])
	latency := time.Since(started).Milliseconds()
	switch {
	case code == 429:
		return "limited", latency, ""
	case code >= 200 && code < 500:
		return "alive", latency, ""
	case code >= 500:
		return "dead", latency, fmt.Sprintf("上游 %d", code)
	default:
		return "dead", latency, fmt.Sprintf("状态 %d", code)
	}
}

// socks5Dial performs a minimal SOCKS5 CONNECT handshake to proxyAddr for the
// target host and port.
func socks5Dial(ctx context.Context, proxyAddr, host string, port int) (net.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if ok {
		conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fail(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fail(errors.New("SOCKS5 认证失败"))
	}
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fail(err)
	}
	if header[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 连接失败: 0x%02x", header[1]))
	}
	switch header[3] {
	case 0x01:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return fail(err)
		}
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(length[0])+2)); err != nil {
			return fail(err)
		}
	case 0x04:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.New("SOCKS5 响应地址类型无效"))
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// handleProxyLogs returns the tail of the sidecar's mihomo log.
func (a *Server) handleProxyLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if limit < 20 || limit > 2000 {
		limit = 300
	}
	logPath := filepath.Join(a.proxyWorkDir(), "mihomo.log")
	if _, err := os.Stat(logPath); err != nil {
		logPath = filepath.Join(a.proxyDir(), "mihomo.log")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleProxyExport streams the merged proxy array or the generated mihomo
// configuration.
func (a *Server) handleProxyExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	name := "merged.json"
	contentType := "application/json"
	if format == "clash" || format == "yaml" {
		name = "mihomo.yaml"
		contentType = "text/yaml"
	}
	data, err := os.ReadFile(filepath.Join(a.proxyDir(), name))
	if err != nil && name == "mihomo.yaml" {
		data, err = os.ReadFile(filepath.Join(a.proxyWorkDir(), name))
	}
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "尚无导出内容，请先同步")
		return
	}
	w.Header().Set("Content-Type", contentType+"; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleProxyImport stores an uploaded Clash YAML as a local provider.
func (a *Server) handleProxyImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	if !strings.Contains(body.Content, "proxies:") {
		writeAdminError(w, http.StatusBadRequest, "invalid_content", "只支持 Clash YAML（需包含 proxies: 列表）")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "手动导入"
	}
	dir := filepath.Join(a.proxyDir(), "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	id := fmt.Sprintf("import-%d", time.Now().UnixNano())
	path := filepath.Join(dir, id+".yaml")
	if err := os.WriteFile(path, []byte(body.Content), 0o600); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	store.Providers = append(store.Providers, ProxyProvider{ID: id, Name: name, URL: path, Enabled: true})
	err := a.saveProxyStore(store)
	a.proxyMu.Unlock()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "path": path})
}
