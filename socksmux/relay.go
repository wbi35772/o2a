package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// relayView is the console view of the subscription-relay side of the pool:
// which subscription node the tunnels attach to, how many tunnels exist, how
// many are healthy and whether their WARP /64 prefixes collide.
type relaySubscription struct {
	Name      string `json:"name"`
	Node      string `json:"node"`
	NodeCount int    `json:"node_count,omitempty"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	SNI       string `json:"sni,omitempty"`
	WSPath    string `json:"ws_path,omitempty"`
	Transport string `json:"transport"`
}

type relayTunnels struct {
	Count      int `json:"count"`
	Available  int `json:"available"`
	Healthy    int `json:"healthy"`
	Failed     int `json:"failed"`
	Duplicates int `json:"duplicates"`
	Unique64   int `json:"unique64"`
	Nodes      int `json:"nodes,omitempty"`
}

type relayNode struct {
	Index     int    `json:"index"`
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Node      string `json:"node,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Loc       string `json:"loc,omitempty"`
	State     string `json:"state"`
	Updated   int64  `json:"updated,omitempty"`
	Duplicate bool   `json:"duplicate"`
}

type relayView struct {
	Enabled      bool              `json:"enabled"`
	StateDir     string            `json:"state_dir"`
	Subscription relaySubscription `json:"subscription"`
	Tunnels      relayTunnels      `json:"tunnels"`
	Regions      []string          `json:"regions"`
	Nodes        []relayNode       `json:"nodes"`
	UpdatedAt    int64             `json:"updated_at,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func readSettingsFile(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// nodes.tsv: label, host, port, sni, ws_path, password per line.
func relayNodeRows(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 6 && strings.TrimSpace(fields[0]) != "" && strings.TrimSpace(fields[5]) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

func countRelayNodes(path string) int { return len(relayNodeRows(path)) }

// firstRelayNode returns the display tuple of the first subscription node.
func firstRelayNode(path string) (host string, port int, sni, ws string) {
	rows := relayNodeRows(path)
	if len(rows) == 0 {
		return "", 0, "", ""
	}
	fields := strings.Split(rows[0], "\t")
	host = strings.TrimSpace(fields[1])
	port, _ = strconv.Atoi(strings.TrimSpace(fields[2]))
	if port <= 0 {
		port = 443
	}
	sni = strings.TrimSpace(fields[3])
	ws = strings.TrimSpace(fields[4])
	return host, port, sni, ws
}

// status.tsv columns: port, pid, endpoint, /64 prefix, loc, state, updated, node.
func (a *apiServer) relayView() relayView {
	dir := strings.TrimSpace(a.relayStateDir)
	if dir == "" {
		dir = "/data/relay-root"
	}
	view := relayView{
		StateDir: dir,
		Regions:  []string{},
		Nodes:    []relayNode{},
	}
	nodesPath := filepath.Join(dir, "relay", "nodes.tsv")
	nodeCount := countRelayNodes(nodesPath)
	settings := readSettingsFile(filepath.Join(dir, "relay", "settings.env"))
	view.Enabled = settings["RELAY_HOST"] != "" || nodeCount > 0 || fileExists(filepath.Join(dir, "relay-mode"))

	nodeHost, nodePort, nodeSNI, nodeWS := firstRelayNode(nodesPath)
	host := settings["RELAY_HOST"]
	if host == "" {
		host = nodeHost
	}
	port := 443
	if v, err := strconv.Atoi(settings["RELAY_PORT"]); err == nil && v > 0 {
		port = v
	} else if nodePort > 0 {
		port = nodePort
	}
	sni := settings["RELAY_SNI"]
	if sni == "" {
		sni = nodeSNI
	}
	ws := settings["RELAY_WS_PATH"]
	if ws == "" {
		ws = nodeWS
	}
	sub := relaySubscription{
		Name:      settings["RELAY_SUB_NAME"],
		Node:      settings["RELAY_NODE_NAME"],
		NodeCount: nodeCount,
		Host:      host,
		Port:      port,
		SNI:       sni,
		WSPath:    ws,
		Transport: "trojan / WebSocket / TLS",
	}
	if sub.Name == "" {
		sub.Name = "家宽订阅（UDP 中继）"
	}
	if sub.Node == "" && sub.NodeCount == 0 {
		sub.Node = sub.Host
	}
	view.Subscription = sub

	data, err := os.ReadFile(filepath.Join(dir, "status.tsv"))
	if err != nil {
		view.Error = err.Error()
		return view
	}

	regions := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(fields) < 6 {
			continue
		}
		node := relayNode{State: strings.TrimSpace(fields[5])}
		node.Port, _ = strconv.Atoi(strings.TrimSpace(fields[0]))
		node.PID, _ = strconv.Atoi(strings.TrimSpace(fields[1]))
		node.Endpoint = strings.TrimSpace(fields[2])
		node.Prefix = strings.TrimSpace(fields[3])
		if node.Prefix == "-" {
			node.Prefix = ""
		}
		node.Loc = strings.TrimSpace(fields[4])
		if node.Loc == "-" {
			node.Loc = ""
		}
		if len(fields) > 6 {
			node.Updated, _ = strconv.ParseInt(strings.TrimSpace(fields[6]), 10, 64)
			if node.Updated > view.UpdatedAt {
				view.UpdatedAt = node.Updated
			}
		}
		if len(fields) > 7 {
			node.Node = strings.TrimSpace(fields[7])
			if node.Node == "-" {
				node.Node = ""
			}
		}
		if node.Loc != "" {
			regions[node.Loc] = true
		}
		node.Index = len(view.Nodes) + 1
		view.Nodes = append(view.Nodes, node)
	}
	sort.Slice(view.Nodes, func(i, j int) bool { return view.Nodes[i].Port < view.Nodes[j].Port })

	usedNodes := map[string]bool{}
	for _, node := range view.Nodes {
		if node.Node != "" {
			usedNodes[node.Node] = true
		}
	}
	view.Tunnels.Nodes = len(usedNodes)

	seen := map[string]int{}
	for _, node := range view.Nodes {
		if node.Prefix != "" {
			seen[node.Prefix]++
		}
	}
	for _, count := range seen {
		if count > 1 {
			view.Tunnels.Duplicates += count - 1
		}
	}
	for i := range view.Nodes {
		node := &view.Nodes[i]
		if node.Prefix != "" && seen[node.Prefix] > 1 {
			node.Duplicate = true
		}
		if node.State == "ok" {
			view.Tunnels.Available++
			if node.Prefix != "" {
				view.Tunnels.Healthy++
			}
		}
	}
	view.Tunnels.Count = len(view.Nodes)
	view.Tunnels.Failed = view.Tunnels.Count - view.Tunnels.Available
	view.Tunnels.Unique64 = len(seen)
	for loc := range regions {
		view.Regions = append(view.Regions, loc)
	}
	sort.Strings(view.Regions)
	return view
}

func (a *apiServer) handleRelay(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.relayView())
}
