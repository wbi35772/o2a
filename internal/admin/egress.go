package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"opencode2api/internal/exitprobe"
	"opencode2api/internal/httpx"
)

// egressWorkerLimit bounds concurrent quota probes. Every probe opens one
// short-lived SOCKS connection per request, so the limit keeps a large
// selection from flooding mihomo while still finishing dozens of exits
// quickly.
const egressWorkerLimit = 8

// egressProbe holds the outcome of probing one listener.
type egressProbe struct {
	Name  string `json:"name"`
	Port  int    `json:"port"`
	Error string `json:"error,omitempty"`
	exitprobe.Result
}

// handleProxyEgress probes the public egress IP and the free-model quota of
// the selected (or filtered) listeners. Each listener is dialled through its
// own SOCKS5 port, so a probe measures the exit the gateway would actually use
// for that entry in the fleet.
func (a *Server) handleProxyEgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names    []string `json:"names"`
		Status   string   `json:"status"`
		Provider string   `json:"provider"`
		Model    string   `json:"model"`
		Limit    int      `json:"limit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil && err != io.EOF {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	if body.Limit <= 0 || body.Limit > 200 {
		body.Limit = 64
	}
	if body.Model == "" {
		body.Model = "beam-free"
	}
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	wanted := map[string]bool{}
	for _, name := range body.Names {
		wanted[name] = true
	}
	targets := make([]ProxyNode, 0, len(store.Nodes))
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
		if len(targets) >= body.Limit {
			break
		}
		targets = append(targets, node)
	}
	timeout := time.Duration(store.Settings.TestTimeoutSec) * time.Second
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	err := a.startProxyJob("egress", func(ctx context.Context) (string, error) {
		results := runEgressProbes(ctx, targets, timeout, body.Model)
		a.proxyMu.Lock()
		current := a.loadProxyStore()
		byName := make(map[string]egressProbe, len(results))
		for _, item := range results {
			byName[item.Name] = item
		}
		ok, limited, blocked, failed := 0, 0, 0, 0
		checked := time.Now().UTC().Format(time.RFC3339)
		for i := range current.Nodes {
			item, found := byName[current.Nodes[i].Name]
			if !found {
				continue
			}
			current.Nodes[i].EgressIP = item.EgressIP
			current.Nodes[i].Quota = item.Quota
			current.Nodes[i].QuotaNote = item.Note
			current.Nodes[i].QuotaCheckedAt = checked
			current.Nodes[i].RetryAfter = item.RetryAfterSeconds
			switch item.Quota {
			case exitprobe.QuotaOK:
				ok++
			case exitprobe.QuotaLimited:
				limited++
			case exitprobe.QuotaBlocked:
				blocked++
			default:
				failed++
			}
		}
		_ = a.saveProxyStore(current)
		a.proxyMu.Unlock()
		return fmt.Sprintf("探测 %d 个出口：可用 %d，限额 %d，封禁 %d，失败 %d",
			len(results), ok, limited, blocked, failed), nil
	})
	a.proxyMu.Unlock()
	if err != nil {
		writeAdminError(w, http.StatusConflict, "proxy_busy", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"started": true, "targets": len(targets)})
}

// runEgressProbes dials every listener over its own SOCKS5 port. Probes run in
// parallel up to egressWorkerLimit; a cancelled context stops the queue.
func runEgressProbes(ctx context.Context, nodes []ProxyNode, timeout time.Duration, model string) []egressProbe {
	results := make([]egressProbe, len(nodes))
	workers := egressWorkerLimit
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
				dial := egressDial(node.ListenPort, timeout)
				res := exitprobe.Probe(ctx, dial, exitprobe.Options{Model: model, Timeout: timeout + 10*time.Second})
				results[idx] = egressProbe{Name: node.Name, Port: node.ListenPort, Result: res}
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

// egressDial returns a dialer that opens a SOCKS5 CONNECT through the local
// mihomo listener owning this exit.
func egressDial(port int, timeout time.Duration) exitprobe.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		target, err := strconv.Atoi(portText)
		if err != nil {
			return nil, err
		}
		return socks5Dial(ctx, fmt.Sprintf("127.0.0.1:%d", port), host, target)
	}
}

// handleCFSourceList returns the saved Cloudflare exit sources (state also
// carries them; this endpoint exists so the tab can refresh alone).
func (a *Server) handleCFSourceList(w http.ResponseWriter, _ *http.Request) {
	a.proxyMu.RLock()
	store := a.loadProxyStore()
	a.proxyMu.RUnlock()
	items := store.CFSources
	if items == nil {
		items = []CFSource{}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sources": items})
}
