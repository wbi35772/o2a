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
}

type relayNode struct {
	Index     int    `json:"index"`
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
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

// status.tsv columns: port, pid, endpoint, /64 prefix, loc, state, updated.
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
	settings := readSettingsFile(filepath.Join(dir, "relay", "settings.env"))
	view.Enabled = settings["RELAY_HOST"] != "" || fileExists(filepath.Join(dir, "relay-mode"))

	port := 443
	if v, err := strconv.Atoi(settings["RELAY_PORT"]); err == nil && v > 0 {
		port = v
	}
	sub := relaySubscription{
		Name:      settings["RELAY_SUB_NAME"],
		Node:      settings["RELAY_NODE_NAME"],
		Host:      settings["RELAY_HOST"],
		Port:      port,
		SNI:       settings["RELAY_SNI"],
		WSPath:    settings["RELAY_WS_PATH"],
		Transport: "trojan / WebSocket / TLS",
	}
	if sub.Name == "" {
		sub.Name = "家宽订阅（UDP 中继）"
	}
	if sub.Node == "" {
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
		if node.Loc != "" {
			regions[node.Loc] = true
		}
		node.Index = len(view.Nodes) + 1
		view.Nodes = append(view.Nodes, node)
	}
	sort.Slice(view.Nodes, func(i, j int) bool { return view.Nodes[i].Port < view.Nodes[j].Port })

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
