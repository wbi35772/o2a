package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/httpx"
)

const (
	poolScopePrivate = "private"
	poolScopePublic  = "public"
)

// poolStatusView is one pool's live /api/v1/status answer. Status is kept as
// raw JSON so the page can render every field the pool exposes without the
// gateway mirroring its schema.
type poolStatusView struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Status json.RawMessage `json:"status,omitempty"`
}

// poolEnv reads the legacy single-pool configuration so the first boot after
// an upgrade seeds the pool tab from the values pool-sync.sh already used.
// Values in POOL_ENV_FILE override the process environment, matching the
// shell script's source order.
func poolEnv(stateDir string) (apiURL, token, scope string) {
	apiURL = strings.TrimSpace(os.Getenv("POOL_API_URL"))
	token = strings.TrimSpace(os.Getenv("POOL_API_TOKEN"))
	scope = strings.TrimSpace(os.Getenv("POOL_SYNC_SCOPE"))
	path := strings.TrimSpace(os.Getenv("POOL_ENV_FILE"))
	if path == "" {
		path = filepath.Join(stateDir, "upgrade", "pool.env")
	}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			switch key {
			case "POOL_API_URL":
				apiURL = value
			case "POOL_API_TOKEN":
				token = value
			case "POOL_SYNC_SCOPE":
				scope = value
			}
		}
	}
	if scope != poolScopePublic {
		scope = poolScopePrivate
	}
	return strings.TrimRight(apiURL, "/"), token, scope
}

// seedPoolsFromEnv migrates a deployment that still configures one pool by
// environment variable. It runs once: PoolsInitialized keeps a later empty
// list from being re-seeded, and the shell sync switches to the stored list.
func (a *Server) seedPoolsFromEnv() {
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	if store.PoolsInitialized {
		return
	}
	apiURL, token, scope := poolEnv(a.stateDir)
	if apiURL == "" {
		return
	}
	store.Pools = []StaticPool{{
		ID:      "env-pool",
		Name:    "环境变量池",
		APIURL:  apiURL,
		Token:   token,
		Scope:   scope,
		Enabled: true,
	}}
	store.PoolsInitialized = true
	if err := a.saveProxyStore(store); err != nil && a.logger != nil {
		a.logger.Warn("seed static pool from environment", "error", err)
	}
}

// egressFilePath is the proxy file the gateway actually loads. The pool tab
// writes the merged endpoint list here so a reload picks it up.
func (a *Server) egressFilePath() string {
	if path := strings.TrimSpace(a.manager.Config().ProxyFile); path != "" {
		return path
	}
	return filepath.Join(a.stateDir, "egress-ext.txt")
}

// poolSyncPath prefers the image copy, which is replaced on every deploy, over
// the copy an older deployment may have left on the data volume.
func poolSyncPath(stateDir string) string {
	if path := strings.TrimSpace(os.Getenv("POOL_SYNC_BIN")); path != "" {
		return path
	}
	for _, candidate := range []string{
		"/usr/local/bin/pool-sync.sh",
		filepath.Join(stateDir, "upgrade", "pool-sync.sh"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func countFileLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}
	return count
}

// syncStaticPools runs the shell fetch, then reloads the gateway only when the
// endpoint list changed. pool-sync.sh exits 0 for unchanged, 2 for changed and
// 1 for a failed fetch, so a transient pool outage never empties the file.
func (a *Server) syncStaticPools(ctx context.Context) (string, error) {
	script := poolSyncPath(a.stateDir)
	if script == "" {
		return "", errors.New("找不到 pool-sync.sh，无法同步静态池")
	}
	cmd := exec.CommandContext(ctx, script, "--fetch-only")
	cmd.Env = append(os.Environ(),
		"STATE_DIR="+a.stateDir,
		"EGRESS_FILE="+a.egressFilePath(),
		"POOL_STATE_FILE="+filepath.Join(a.proxyDir(), "state.json"),
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return string(out), err
		}
	}
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	store.PoolSync.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	store.PoolSync.Endpoints = countFileLines(a.egressFilePath())
	switch code {
	case 0:
		store.PoolSync.Error = ""
	case 2:
		if _, reloadErr := a.manager.Reload(); reloadErr != nil {
			store.PoolSync.Error = truncateText(reloadErr.Error(), 500)
			_ = a.saveProxyStore(store)
			a.proxyMu.Unlock()
			return string(out), fmt.Errorf("端点已更新，但网关重载失败: %w", reloadErr)
		}
		store.PoolSync.Error = ""
	default:
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = "池同步失败，已保留现有端点列表"
		}
		store.PoolSync.Error = truncateText(message, 500)
	}
	_ = a.saveProxyStore(store)
	endpoints := store.PoolSync.Endpoints
	syncError := store.PoolSync.Error
	a.proxyMu.Unlock()
	if code != 0 && code != 2 {
		return string(out), errors.New(syncError)
	}
	if code == 2 {
		return fmt.Sprintf("已同步 %d 个端点并重载网关", endpoints), nil
	}
	return fmt.Sprintf("端点无变化（%d 个）", endpoints), nil
}

func (a *Server) findPool(id string) (StaticPool, bool) {
	a.proxyMu.RLock()
	defer a.proxyMu.RUnlock()
	for _, pool := range a.loadProxyStore().Pools {
		if pool.ID == id {
			return pool, true
		}
	}
	return StaticPool{}, false
}

func (a *Server) poolHTTP(ctx context.Context, pool StaticPool, method, path string, body []byte) ([]byte, error) {
	base := strings.TrimRight(strings.TrimSpace(pool.APIURL), "/")
	if base == "" {
		return nil, errors.New("API 地址为空")
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if pool.Token != "" {
		req.Header.Set("Authorization", "Bearer "+pool.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateText(message, 300))
	}
	return data, nil
}

func (a *Server) fetchPoolStatus(ctx context.Context, pool StaticPool) poolStatusView {
	view := poolStatusView{ID: pool.ID, Name: pool.Name}
	data, err := a.poolHTTP(ctx, pool, http.MethodGet, "/api/v1/status", nil)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if !json.Valid(data) {
		view.Error = "池返回了非 JSON 状态"
		return view
	}
	view.OK = true
	view.Status = json.RawMessage(data)
	return view
}

// maskPoolSecrets copies the list for the page: the token is never returned,
// only whether one is stored, so an empty token on save means "keep it".
func maskPoolSecrets(pools []StaticPool) []StaticPool {
	out := make([]StaticPool, len(pools))
	for i, pool := range pools {
		pool.TokenSet = pool.Token != ""
		pool.Token = ""
		out[i] = pool
	}
	return out
}

// handlePoolList returns the saved pool list without contacting the pools.
func (a *Server) handlePoolList(w http.ResponseWriter, _ *http.Request) {
	a.proxyMu.RLock()
	store := a.loadProxyStore()
	a.proxyMu.RUnlock()
	pools := maskPoolSecrets(store.Pools)
	if pools == nil {
		pools = []StaticPool{}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pools":        pools,
		"pool_sync":    store.PoolSync,
		"egress_file":  a.egressFilePath(),
		"egress_lines": countFileLines(a.egressFilePath()),
	})
}

// handlePoolStatus contacts every enabled pool and returns its live status
// document plus its runtime tuning values and node list.
func (a *Server) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	a.proxyMu.RLock()
	pools := append([]StaticPool(nil), a.loadProxyStore().Pools...)
	a.proxyMu.RUnlock()
	views := make([]poolStatusView, len(pools))
	var wg sync.WaitGroup
	for i := range pools {
		if !pools[i].Enabled {
			views[i] = poolStatusView{ID: pools[i].ID, Name: pools[i].Name, Error: "已停用"}
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			views[index] = a.fetchPoolStatus(r.Context(), pools[index])
		}(i)
	}
	wg.Wait()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"pools":        views,
		"egress_file":  a.egressFilePath(),
		"egress_lines": countFileLines(a.egressFilePath()),
	})
}

// handlePoolSave replaces the whole list. An empty token on an existing id
// keeps the stored secret so the page can render it masked and still save.
func (a *Server) handlePoolSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pools []StaticPool `json:"pools"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	existing := make(map[string]StaticPool, len(store.Pools))
	for _, pool := range store.Pools {
		existing[pool.ID] = pool
	}
	seen := make(map[string]bool, len(body.Pools))
	cleaned := make([]StaticPool, 0, len(body.Pools))
	for index, pool := range body.Pools {
		pool.ID = strings.TrimSpace(pool.ID)
		pool.Name = strings.TrimSpace(pool.Name)
		pool.APIURL = strings.TrimRight(strings.TrimSpace(pool.APIURL), "/")
		pool.Token = strings.TrimSpace(pool.Token)
		pool.Scope = strings.ToLower(strings.TrimSpace(pool.Scope))
		if pool.ID == "" {
			pool.ID = fmt.Sprintf("pool-%x", time.Now().UnixNano()+int64(index))
		}
		if seen[pool.ID] {
			writeAdminError(w, http.StatusBadRequest, "duplicate_id", "池 ID 重复")
			return
		}
		seen[pool.ID] = true
		if pool.Name == "" || len(pool.Name) > 80 {
			writeAdminError(w, http.StatusBadRequest, "invalid_name", "池名称需为 1-80 个字符")
			return
		}
		if pool.APIURL == "" || len(pool.APIURL) > 2048 ||
			(!strings.HasPrefix(pool.APIURL, "http://") && !strings.HasPrefix(pool.APIURL, "https://")) {
			writeAdminError(w, http.StatusBadRequest, "invalid_url", "API 地址需为 http(s) URL")
			return
		}
		if pool.Scope == "" {
			pool.Scope = poolScopePrivate
		}
		if pool.Scope != poolScopePrivate && pool.Scope != poolScopePublic {
			writeAdminError(w, http.StatusBadRequest, "invalid_scope", "端点范围只能是 private 或 public")
			return
		}
		if pool.Token == "" {
			if old, ok := existing[pool.ID]; ok {
				pool.Token = old.Token
			}
		}
		if old, ok := existing[pool.ID]; ok {
			pool.EndpointCount = old.EndpointCount
			pool.LastSync = old.LastSync
			pool.LastError = old.LastError
		}
		cleaned = append(cleaned, pool)
	}
	store.Pools = cleaned
	store.PoolsInitialized = true
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pools": maskPoolSecrets(cleaned)})
}

func (a *Server) handlePoolSync(w http.ResponseWriter, _ *http.Request) {
	err := a.startProxyJob("pool-sync", func(ctx context.Context) (string, error) {
		return a.syncStaticPools(ctx)
	})
	if err != nil {
		writeAdminError(w, http.StatusConflict, "proxy_busy", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// handlePoolConfig forwards the tuning fields the pool hot-reloads. Omitted
// fields keep their pool-side values; min_healthy accepts zero.
func (a *Server) handlePoolConfig(w http.ResponseWriter, r *http.Request) {
	pool, ok := a.findPool(r.PathValue("id"))
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "池不存在")
		return
	}
	var body struct {
		RotationIntervalSeconds *int `json:"rotation_interval_seconds"`
		RerollBatch             *int `json:"reroll_batch"`
		HealthIntervalSeconds   *int `json:"health_interval_seconds"`
		MinHealthy              *int `json:"min_healthy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	patch := map[string]int{}
	if body.RotationIntervalSeconds != nil {
		if *body.RotationIntervalSeconds < 60 || *body.RotationIntervalSeconds > 86400 {
			writeAdminError(w, http.StatusBadRequest, "invalid_rotation", "轮换间隔需为 60-86400 秒")
			return
		}
		patch["rotation_interval_seconds"] = *body.RotationIntervalSeconds
	}
	if body.RerollBatch != nil {
		if *body.RerollBatch < 1 || *body.RerollBatch > 10000 {
			writeAdminError(w, http.StatusBadRequest, "invalid_batch", "每轮轮换数无效")
			return
		}
		patch["reroll_batch"] = *body.RerollBatch
	}
	if body.HealthIntervalSeconds != nil {
		if *body.HealthIntervalSeconds < 30 || *body.HealthIntervalSeconds > 86400 {
			writeAdminError(w, http.StatusBadRequest, "invalid_health", "健康检查间隔需为 30-86400 秒")
			return
		}
		patch["health_interval_seconds"] = *body.HealthIntervalSeconds
	}
	if body.MinHealthy != nil {
		if *body.MinHealthy < 0 || *body.MinHealthy > 10000 {
			writeAdminError(w, http.StatusBadRequest, "invalid_min_healthy", "最小健康数无效")
			return
		}
		patch["min_healthy"] = *body.MinHealthy
	}
	if len(patch) == 0 {
		writeAdminError(w, http.StatusBadRequest, "empty_patch", "没有可保存的参数")
		return
	}
	payload, _ := json.Marshal(patch)
	data, err := a.poolHTTP(r.Context(), pool, http.MethodPut, "/api/v1/config", payload)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "pool_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if json.Valid(data) {
		_, _ = w.Write(data)
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (a *Server) handlePoolReroll(w http.ResponseWriter, r *http.Request) {
	pool, ok := a.findPool(r.PathValue("id"))
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "池不存在")
		return
	}
	data, err := a.poolHTTP(r.Context(), pool, http.MethodPost, "/api/v1/reroll", nil)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "pool_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if json.Valid(data) {
		_, _ = w.Write(data)
		return
	}
	_, _ = w.Write([]byte(`{"queued":true}`))
}

func (a *Server) handlePoolNode(w http.ResponseWriter, r *http.Request) {
	pool, ok := a.findPool(r.PathValue("id"))
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "池不存在")
		return
	}
	user := r.PathValue("user")
	action := r.PathValue("action")
	switch action {
	case "rotate", "enable", "disable":
	default:
		writeAdminError(w, http.StatusBadRequest, "invalid_action", "节点操作只能是 rotate/enable/disable")
		return
	}
	path := "/api/v1/nodes/" + url.PathEscape(user) + "/" + action
	data, err := a.poolHTTP(r.Context(), pool, http.MethodPost, path, nil)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "pool_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if json.Valid(data) {
		_, _ = w.Write(data)
		return
	}
	_, _ = w.Write([]byte(`{"queued":true}`))
}

func (a *Server) handlePoolEndpoints(w http.ResponseWriter, r *http.Request) {
	pool, ok := a.findPool(r.PathValue("id"))
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "池不存在")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	if scope != poolScopePublic {
		scope = poolScopePrivate
	}
	data, err := a.poolHTTP(r.Context(), pool, http.MethodGet, "/api/v1/endpoints?format=json&scope="+scope, nil)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "pool_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *Server) handlePoolLogs(w http.ResponseWriter, r *http.Request) {
	pool, ok := a.findPool(r.PathValue("id"))
	if !ok {
		writeAdminError(w, http.StatusNotFound, "not_found", "池不存在")
		return
	}
	lines := 200
	if value := r.URL.Query().Get("lines"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 && parsed <= 2000 {
			lines = parsed
		}
	}
	data, err := a.poolHTTP(r.Context(), pool, http.MethodGet, fmt.Sprintf("/api/v1/logs?lines=%d", lines), nil)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "pool_error", err.Error())
		return
	}
	items := []string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			items = append(items, line)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"lines": items})
}
