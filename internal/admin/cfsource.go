package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"opencode2api/internal/httpx"
)

// CFSource is one Cloudflare Worker deployment used as an exit source. Each
// Egress entry (a PROXYIP host:port, a SOCKS5 URL, or any value the worker
// understands in its ws path) becomes one VLESS node, so a single Worker can
// fan out into many distinct egress IPs instead of the one IP a plain
// subscription exposes.
type CFSource struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Host         string   `json:"host"`
	UUID         string   `json:"uuid"`
	Port         int      `json:"port"`
	ServerName   string   `json:"servername,omitempty"`
	HostHeader   string   `json:"host_header,omitempty"`
	PathTemplate string   `json:"path_template"`
	Edge         []string `json:"edge,omitempty"`
	Egress       []string `json:"egress,omitempty"`
	Enabled      bool     `json:"enabled"`
	ProviderID   string   `json:"provider_id,omitempty"`
	NodeCount    int      `json:"node_count"`
	LastBuild    string   `json:"last_build,omitempty"`
	LastError    string   `json:"last_error,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
}

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._:-]*[A-Za-z0-9])?$`)
)

// defaultCFEdges are Cloudflare's public anycast addresses. They are only the
// connection entry point; the egress IP is decided by the ws path.
var defaultCFEdges = []string{
	"104.18.33.247", "104.17.25.215", "104.19.2.232", "172.67.154.171",
	"162.159.148.206", "104.17.138.137", "104.19.147.223", "104.19.148.252",
}

const (
	defaultCFPath = "/proxyip=<ip>"
	maxCFEgress   = 5000
	maxCFEdge     = 64
)

func normalizeCFSource(src *CFSource) error {
	src.Name = strings.TrimSpace(src.Name)
	src.Host = normalizeWorkerHost(src.Host)
	src.UUID = strings.ToLower(strings.TrimSpace(src.UUID))
	src.ServerName = strings.TrimSpace(src.ServerName)
	src.HostHeader = strings.TrimSpace(src.HostHeader)
	src.PathTemplate = strings.TrimSpace(src.PathTemplate)
	if src.Name == "" || len([]rune(src.Name)) > 80 {
		return errors.New("名称需为 1–80 个字符")
	}
	if src.Host == "" || len(src.Host) > 253 || !hostPattern.MatchString(src.Host) {
		return errors.New("Worker 域名无效")
	}
	if !uuidPattern.MatchString(src.UUID) {
		return errors.New("UUID 无效")
	}
	if src.Port == 0 {
		src.Port = 443
	}
	if src.Port < 1 || src.Port > 65535 {
		return errors.New("端口需在 1–65535 之间")
	}
	if src.ServerName == "" {
		src.ServerName = src.Host
	}
	if src.HostHeader == "" {
		src.HostHeader = src.Host
	}
	if src.PathTemplate == "" {
		src.PathTemplate = defaultCFPath
	}
	if !strings.HasPrefix(src.PathTemplate, "/") || len(src.PathTemplate) > 512 {
		return errors.New("路径模板需以 / 开头且不超过 512 个字符")
	}
	src.Edge = cleanCFList(src.Edge, maxCFEdge)
	for _, edge := range src.Edge {
		if strings.ContainsAny(edge, " /?#") || !hostPattern.MatchString(edge) {
			return fmt.Errorf("边缘地址无效: %s", edge)
		}
	}
	egress := cleanCFList(src.Egress, maxCFEgress)
	for _, item := range egress {
		if strings.ContainsAny(item, " \t\r\n") || len(item) > 300 {
			return fmt.Errorf("出口参数无效: %s", item)
		}
	}
	src.Egress = egress
	if len(src.Egress) > 0 && !strings.Contains(src.PathTemplate, "<ip>") && !strings.Contains(src.PathTemplate, "{ip}") {
		return errors.New("路径模板必须包含 <ip> 或 {ip} 占位符")
	}
	return nil
}

// normalizeWorkerHost accepts a pasted URL or bare hostname and returns the
// hostname only.
func normalizeWorkerHost(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSuffix(raw, ".")
}

func cleanCFList(items []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		for _, part := range strings.FieldsFunc(item, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

type cfWSHeaders struct {
	Host string `json:"Host"`
}

type cfWSOpts struct {
	Path    string      `json:"path"`
	Headers cfWSHeaders `json:"headers"`
}

type cfProxy struct {
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Server            string   `json:"server"`
	Port              int      `json:"port"`
	UUID              string   `json:"uuid"`
	TLS               bool     `json:"tls"`
	ServerName        string   `json:"servername,omitempty"`
	Network           string   `json:"network"`
	WSOpts            cfWSOpts `json:"ws-opts"`
	ClientFingerprint string   `json:"client-fingerprint"`
	SkipCertVerify    bool     `json:"skip-cert-verify"`
	UDP               bool     `json:"udp"`
}

type cfManifest struct {
	Proxies []cfProxy `json:"proxies"`
}

// buildCFManifest turns one deployment into a Clash proxy list. With no
// Egress values it keeps the worker's own egress (a single direct node).
func buildCFManifest(src CFSource) (cfManifest, error) {
	edges := src.Edge
	if len(edges) == 0 {
		edges = defaultCFEdges
	}
	slug := src.ID
	if len(slug) > 8 {
		slug = slug[:8]
	}
	if slug == "" {
		slug = "cf"
	}
	manifest := cfManifest{Proxies: make([]cfProxy, 0, len(src.Egress))}
	render := func(index int, egress string) cfProxy {
		path := src.PathTemplate
		if egress != "" {
			path = strings.ReplaceAll(path, "<ip>", egress)
			path = strings.ReplaceAll(path, "{ip}", egress)
		} else {
			path = "/"
		}
		return cfProxy{
			Name:              fmt.Sprintf("cf-%s-%d", slug, index),
			Type:              "vless",
			Server:            edges[index%len(edges)],
			Port:              src.Port,
			UUID:              src.UUID,
			TLS:               true,
			ServerName:        src.ServerName,
			Network:           "ws",
			WSOpts:            cfWSOpts{Path: path, Headers: cfWSHeaders{Host: src.HostHeader}},
			ClientFingerprint: "chrome",
			SkipCertVerify:    false,
			UDP:               false,
		}
	}
	if len(src.Egress) == 0 {
		manifest.Proxies = append(manifest.Proxies, render(0, ""))
		return manifest, nil
	}
	for i, egress := range src.Egress {
		manifest.Proxies = append(manifest.Proxies, render(i, egress))
	}
	return manifest, nil
}

// writeCFManifest persists the generated Clash file and returns its path.
func (a *Server) writeCFManifest(src CFSource) (string, int, error) {
	manifest, err := buildCFManifest(src)
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(a.proxyDir(), "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, "cf-"+src.ID+".yaml")
	if err := writePrivateFile(path, data); err != nil {
		return "", 0, err
	}
	return path, len(manifest.Proxies), nil
}

// attachCFProvider keeps the ordinary provider list pointed at the generated
// file so sync/fetch works exactly like any other subscription.
func attachCFProvider(store *ProxyStore, src *CFSource, path string, count int) {
	providerID := "cf-" + src.ID
	src.ProviderID = providerID
	src.NodeCount = count
	src.LastBuild = time.Now().UTC().Format(time.RFC3339)
	src.LastError = ""
	entry := ProxyProvider{
		ID: providerID, Name: "CF · " + src.Name, URL: path,
		Enabled: src.Enabled, NodeCount: count, LastFetch: src.LastBuild,
	}
	for i := range store.Providers {
		if store.Providers[i].ID == providerID {
			store.Providers[i] = entry
			return
		}
	}
	store.Providers = append(store.Providers, entry)
}

func (a *Server) saveCFSource(store *ProxyStore, src *CFSource) error {
	path, count, err := a.writeCFManifest(*src)
	if err != nil {
		src.LastError = err.Error()
		return err
	}
	attachCFProvider(store, src, path, count)
	return nil
}

func (a *Server) handleCFSourceCreate(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	var src CFSource
	if err := json.Unmarshal(data, &src); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	// Enabled is a plain bool, so an omitted field would otherwise read as
	// "disabled"; a new source is created enabled unless the caller says no.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		if _, ok := fields["enabled"]; !ok {
			src.Enabled = true
		}
	}
	if err := normalizeCFSource(&src); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_source", err.Error())
		return
	}
	src.ID = fmt.Sprintf("%x", time.Now().UnixNano())
	src.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	a.proxyMu.Lock()
	store := a.loadProxyStore()
	if err := a.saveCFSource(&store, &src); err != nil {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "build_failed", err.Error())
		return
	}
	store.CFSources = append(store.CFSources, src)
	if err := a.saveProxyStore(store); err != nil {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if err := a.writeProvidersFile(store); err != nil {
		a.proxyMu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	a.proxyMu.Unlock()
	httpx.WriteJSON(w, http.StatusCreated, src)
}

func (a *Server) handleCFSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	var body CFSource
	if err := json.Unmarshal(data, &body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	body.ID = id
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	index := -1
	for i := range store.CFSources {
		if store.CFSources[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
		return
	}
	body.CreatedAt = store.CFSources[index].CreatedAt
	body.ProviderID = store.CFSources[index].ProviderID
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		if _, ok := fields["enabled"]; !ok {
			body.Enabled = store.CFSources[index].Enabled
		}
	}
	if err := normalizeCFSource(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_source", err.Error())
		return
	}
	if err := a.saveCFSource(&store, &body); err != nil {
		store.CFSources[index].LastError = err.Error()
		_ = a.saveProxyStore(store)
		writeAdminError(w, http.StatusInternalServerError, "build_failed", err.Error())
		return
	}
	store.CFSources[index] = body
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if err := a.writeProvidersFile(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (a *Server) handleCFSourceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	index := -1
	for i := range store.CFSources {
		if store.CFSources[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
		return
	}
	providerID := store.CFSources[index].ProviderID
	if providerID == "" {
		providerID = "cf-" + id
	}
	store.CFSources = append(store.CFSources[:index], store.CFSources[index+1:]...)
	kept := store.Providers[:0]
	for _, provider := range store.Providers {
		if provider.ID != providerID {
			kept = append(kept, provider)
		}
	}
	store.Providers = kept
	_ = os.Remove(filepath.Join(a.proxyDir(), "imports", "cf-"+id+".yaml"))
	if err := a.saveProxyStore(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if err := a.writeProvidersFile(store); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleCFSourceBuild regenerates the node file without refetching.
func (a *Server) handleCFSourceBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.proxyMu.Lock()
	defer a.proxyMu.Unlock()
	store := a.loadProxyStore()
	for i := range store.CFSources {
		if store.CFSources[i].ID != id {
			continue
		}
		src := store.CFSources[i]
		if err := a.saveCFSource(&store, &src); err != nil {
			store.CFSources[i].LastError = err.Error()
			_ = a.saveProxyStore(store)
			writeAdminError(w, http.StatusInternalServerError, "build_failed", err.Error())
			return
		}
		store.CFSources[i] = src
		if err := a.saveProxyStore(store); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, src)
		return
	}
	writeAdminError(w, http.StatusNotFound, "not_found", "Cloudflare 出口不存在")
}

// cfDeployParams is the result of importing a Cloudflare deployment snippet.
type cfDeployParams struct {
	UUID         string   `json:"uuid"`
	Host         string   `json:"host"`
	Key          string   `json:"key,omitempty"`
	ProxyIP      []string `json:"proxyip"`
	Socks5       []string `json:"socks5"`
	PathTemplate string   `json:"path_template"`
}

// handleCFParseDeploy accepts the env block or JSON pasted from the Cloudflare
// dashboard and extracts the fields the panel needs.
func (a *Server) handleCFParseDeploy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_body", "请求体无效")
		return
	}
	params, err := parseCFDeploy(body.Text)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_params", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, params)
}

func parseCFDeploy(text string) (cfDeployParams, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return cfDeployParams{}, errors.New("部署参数为空")
	}
	values := map[string]string{}
	if strings.HasPrefix(text, "{") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return cfDeployParams{}, fmt.Errorf("JSON 解析失败: %w", err)
		}
		for key, value := range raw {
			values[strings.ToUpper(strings.TrimSpace(key))] = cfValueString(value)
		}
	} else {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.ToUpper(strings.TrimSpace(key))
			key = strings.Trim(key, `"'`)
			value = strings.TrimSpace(value)
			value = strings.Trim(value, `"'`)
			if prev, exists := values[key]; exists && prev != "" {
				values[key] = prev + "," + value
			} else {
				values[key] = value
			}
		}
	}
	params := cfDeployParams{
		UUID:    values["UUID"],
		Host:    normalizeWorkerHost(firstNonEmpty(values["HOST"], values["WORKER_HOST"], values["DOMAIN"])),
		Key:     values["KEY"],
		ProxyIP: cleanCFList([]string{values["PROXYIP"]}, maxCFEgress),
		Socks5:  cleanCFList([]string{values["SOCKS5"], values["GO2SOCKS5"]}, maxCFEgress),
	}
	if params.UUID == "" {
		return cfDeployParams{}, errors.New("未找到 UUID")
	}
	if !uuidPattern.MatchString(strings.TrimSpace(params.UUID)) {
		return cfDeployParams{}, errors.New("UUID 格式无效")
	}
	params.UUID = strings.ToLower(strings.TrimSpace(params.UUID))
	params.PathTemplate = defaultCFPath
	if len(params.Socks5) > 0 && len(params.ProxyIP) == 0 {
		params.PathTemplate = "/socks5=<ip>"
	}
	if params.Host == "" {
		return cfDeployParams{}, errors.New("未找到 HOST（Worker 域名）")
	}
	return params, nil
}

func cfValueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// sortCFSources gives the panel a stable order.
func sortCFSources(items []CFSource) {
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
}
