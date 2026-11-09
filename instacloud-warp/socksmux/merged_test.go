package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeNode struct {
	user, prefix, state string
	enabled             bool
}

func fakePool(t *testing.T, poolID, privateHost string, port int, nodes []fakeNode) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{
			"pool": map[string]any{
				"id":           poolID,
				"private_host": privateHost,
				"public_host":  "pub.example",
				"mux_port":     port,
				"count":        len(nodes),
			},
		}
		list := make([]map[string]any, 0, len(nodes))
		for i, n := range nodes {
			list = append(list, map[string]any{
				"user":     n.user,
				"index":    i + 1,
				"port":     1082 + i,
				"state":    n.state,
				"enabled":  n.enabled,
				"prefix64": n.prefix,
			})
		}
		doc["nodes"] = list
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeUpstreams(t *testing.T, specs []upstreamSpec) string {
	t.Helper()
	dir := t.TempDir()
	ctl := filepath.Join(dir, "control")
	if err := os.MkdirAll(ctl, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ctl, "upstreams.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMergedDedupAndDeterminism(t *testing.T) {
	eu1 := fakePool(t, "eu-1", "pool1.internal", 1081, []fakeNode{
		{user: "warpuser07", prefix: "2a09:bac5:27b8::/64", state: "ok", enabled: true},
		{user: "warpuser01", prefix: "2a09:bac5:27b8::/64", state: "ok", enabled: true},
		{user: "warpuser03", prefix: "2a09:bac5:27b9::/64", state: "ok", enabled: true},
		{user: "warpuser04", prefix: "2a09:bac5:27ba::/64", state: "bad", enabled: true},
		{user: "warpuser05", prefix: "2a09:bac5:27bb::/64", state: "ok", enabled: false},
	})
	eu2 := fakePool(t, "eu-2", "pool2.internal", 1081, []fakeNode{
		{user: "warpuser01", prefix: "2a09:bac5:27b9::/64", state: "ok", enabled: true},
		{user: "warpuser09", prefix: "2a09:bac5:27bc::/64", state: "ok", enabled: true},
	})
	us := fakePool(t, "us-1", "poliad.internal", 1081, []fakeNode{
		{user: "warpuser02", prefix: "2a09:bac1:7681:62b0::/64", state: "ok", enabled: true},
	})

	specs := []upstreamSpec{
		{Name: "us", BaseURL: us.URL, OverrideHost: "poliad.internal", OverridePort: 1081},
		{Name: "eu-2", BaseURL: eu2.URL, OverrideHost: "pool2.internal", OverridePort: 1081},
		{Name: "eu-1", BaseURL: eu1.URL, OverrideHost: "pool1.internal", OverridePort: 1081},
		{Name: "off", BaseURL: "http://127.0.0.1:1", Enabled: boolPtr(false)},
	}
	path := writeUpstreams(t, specs)
	hub := newMergedHub("", path, "warpuser", func(upstreamSpec) string { return "secret" })

	snap, err := hub.snapshot(true)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Unique64 != 4 {
		t.Fatalf("unique64 = %d, want 4 (got %+v)", snap.Unique64, nodesDebug(snap.Nodes))
	}
	if snap.Endpoints != 4 {
		t.Fatalf("endpoints = %d, want 4", snap.Endpoints)
	}

	byPrefix := map[string]mergedNode{}
	for _, n := range snap.Nodes {
		byPrefix[n.Prefix64] = n
	}

	n := byPrefix["2a09:bac5:27b8::/64"]
	if n.User != "warpuser01" || n.Upstream != "eu-1" {
		t.Fatalf("27b8 picked %s/%s, want eu-1/warpuser01 (lexicographic tiebreak)", n.Upstream, n.User)
	}
	if n.Host != "pool1.internal" || n.Port != 1081 {
		t.Fatalf("27b8 host/port = %s:%d, want pool1.internal:1081", n.Host, n.Port)
	}
	wantURL := "socks5://warpuser01:secret@pool1.internal:1081"
	if n.URL != wantURL {
		t.Fatalf("url = %q, want %q", n.URL, wantURL)
	}

	n = byPrefix["2a09:bac5:27b9::/64"]
	if n.Upstream != "eu-1" || n.User != "warpuser03" {
		t.Fatalf("27b9 picked %s/%s, want eu-1/warpuser03 (name before user in sort)", n.Upstream, n.User)
	}

	if _, ok := byPrefix["2a09:bac5:27ba::/64"]; ok {
		t.Fatal("bad-state node leaked into snapshot")
	}
	if _, ok := byPrefix["2a09:bac5:27bb::/64"]; ok {
		t.Fatal("disabled node leaked into snapshot")
	}

	for i := 0; i < 5; i++ {
		again, err := hub.snapshot(true)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := endpointsText(again), endpointsText(snap); got != want {
			t.Fatalf("run %d not deterministic:\n%s\nvs\n%s", i, got, want)
		}
	}

	// polling cache: default ttl returns identical pointer content without refetch
	c1, _ := hub.snapshot(false)
	c2, _ := hub.snapshot(false)
	if c1 != c2 {
		t.Fatal("cache did not return the same snapshot")
	}

	usNode := byPrefix["2a09:bac1:7681:62b0::/64"]
	if usNode.Host != "poliad.internal" {
		t.Fatalf("us override host = %s", usNode.Host)
	}
}

func TestMergedWithoutFile(t *testing.T) {
	hub := newMergedHub(t.TempDir(), "", "warpuser", nil)
	if _, err := hub.snapshot(true); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestMergedPartialUpstreamFailure(t *testing.T) {
	good := fakePool(t, "good", "good.internal", 1081, []fakeNode{
		{user: "warpuser01", prefix: "2a09:bac5:27b8::/64", state: "ok", enabled: true},
	})
	specs := []upstreamSpec{
		{Name: "down", BaseURL: "http://127.0.0.1:1"},
		{Name: "good", BaseURL: good.URL, OverrideHost: "good.internal", OverridePort: 1081},
	}
	hub := newMergedHub("", writeUpstreams(t, specs), "warpuser", func(upstreamSpec) string { return "pw" })
	snap, err := hub.snapshot(true)
	if err != nil {
		t.Fatalf("partial failure should still serve: %v", err)
	}
	if snap.Unique64 != 1 || snap.Endpoints != 1 {
		t.Fatalf("want 1 endpoint from healthy upstream, got %+v", snap)
	}
	var downOK, downErr bool
	for _, u := range snap.Upstreams {
		if u.Name == "down" {
			downErr = u.Error != ""
			downOK = u.OK
		}
	}
	if downOK || !downErr {
		t.Fatalf("down upstream view wrong: %+v", snap.Upstreams)
	}
}

func boolPtr(b bool) *bool { return &b }

func endpointsText(s *mergedSnapshot) string {
	parts := make([]string, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		parts = append(parts, fmt.Sprintf("%s=%s/%s@%s:%d", n.Prefix64, n.Upstream, n.User, n.Host, n.Port))
	}
	return strings.Join(parts, "\n")
}

func nodesDebug(nodes []mergedNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Prefix64+" "+n.Upstream+" "+n.User)
	}
	return out
}
