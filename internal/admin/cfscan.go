package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"opencode2api/internal/edgescan"
	"opencode2api/internal/httpx"
)

// handleCFSourceScan samples Cloudflare edge addresses, validates them against
// this Worker, and stores the fastest ones as the source's entry list. It is a
// synchronous call: 200 candidates with a 1.5s timeout finish in a few seconds.
func (a *Server) handleCFSourceScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Candidates int `json:"candidates"`
		TimeoutMS  int `json:"timeout_ms"`
		Keep       int `json:"keep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	if body.Candidates <= 0 {
		body.Candidates = 200
	}
	if body.Candidates > 1000 {
		writeAdminError(w, http.StatusBadRequest, "invalid_candidates", "扫描数量需在 1–1000 之间")
		return
	}
	if body.Keep <= 0 {
		body.Keep = 8
	}
	if body.Keep > 32 {
		writeAdminError(w, http.StatusBadRequest, "invalid_keep", "保留数量需在 1–32 之间")
		return
	}
	timeout := time.Duration(body.TimeoutMS) * time.Millisecond
	if timeout < 500*time.Millisecond || timeout > 5*time.Second {
		timeout = 1500 * time.Millisecond
	}

	a.proxyMu.RLock()
	store := a.loadProxyStore()
	src, found := findCFSource(store, id)
	a.proxyMu.RUnlock()
	if !found {
		writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
		return
	}
	report, err := edgescan.Scan(r.Context(), edgescan.Options{
		Host:        src.Host,
		Port:        src.Port,
		UUID:        src.UUID,
		Candidates:  body.Candidates,
		Keep:        body.Keep,
		Timeout:     timeout,
		Concurrency: 32,
	})
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "scan_failed", err.Error())
		return
	}
	if len(report.Hits) == 0 {
		writeAdminError(w, http.StatusBadGateway, "scan_empty", "没有发现可用边缘 IP，请稍后重试")
		return
	}
	edges := make([]string, 0, len(report.Hits))
	for _, hit := range report.Hits {
		edges = append(edges, hit.IP)
	}

	a.proxyMu.Lock()
	store = a.loadProxyStore()
	src, found = findCFSource(store, id)
	if !found {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
		return
	}
	index := -1
	for i := range store.CFSources {
		if store.CFSources[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
		return
	}
	src.Edge = edges
	if err := a.saveCFSource(&store, &src); err != nil {
		store.CFSources[index].LastError = err.Error()
		_ = a.saveProxyStore(store)
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "build_failed", err.Error())
		return
	}
	store.CFSources[index] = src
	if err := a.saveProxyStore(store); err != nil {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	a.proxyMu.Unlock()
	// Fold the new entry list into the running fleet right away. If another
	// job owns the guard the operator can still press 同步订阅 later.
	syncStarted := a.startProxySyncJob() == nil
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"source": src, "report": report, "sync_started": syncStarted,
	})
}

func findCFSource(store ProxyStore, id string) (CFSource, bool) {
	for _, src := range store.CFSources {
		if src.ID == id {
			return src, true
		}
	}
	return CFSource{}, false
}
