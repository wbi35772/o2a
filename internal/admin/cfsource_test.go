package admin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCFDeployEnvBlock(t *testing.T) {
	text := `UUID=9679f5cf-6da5-4f2b-9f3e-39278492a4e2
KEY=88815e1fc3ae45d4
HOST=https://edgetunnel-full.wbi35772.workers.dev/sub?token=x
PROXYIP=1.2.3.4:443, 5.6.7.8:443
PROXYIP=9.9.9.9:443`
	params, err := parseCFDeploy(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if params.Host != "edgetunnel-full.wbi35772.workers.dev" {
		t.Fatalf("host = %q", params.Host)
	}
	if params.UUID != "9679f5cf-6da5-4f2b-9f3e-39278492a4e2" {
		t.Fatalf("uuid = %q", params.UUID)
	}
	if len(params.ProxyIP) != 3 {
		t.Fatalf("proxyip = %v", params.ProxyIP)
	}
	if params.PathTemplate != "/proxyip=<ip>" {
		t.Fatalf("template = %q", params.PathTemplate)
	}
}

func TestParseCFDeploySocks5(t *testing.T) {
	text := `{"UUID":"9679f5cf-6da5-4f2b-9f3e-39278492a4e2","HOST":"a.example.workers.dev","SOCKS5":"user:pass@1.1.1.1:1080"}`
	params, err := parseCFDeploy(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(params.Socks5) != 1 || params.PathTemplate != "/socks5=<ip>" {
		t.Fatalf("params = %+v", params)
	}
}

func TestParseCFDeployRejectsMissing(t *testing.T) {
	if _, err := parseCFDeploy("HOST=a.example.workers.dev"); err == nil {
		t.Fatal("expected missing UUID error")
	}
	if _, err := parseCFDeploy("UUID=9679f5cf-6da5-4f2b-9f3e-39278492a4e2"); err == nil {
		t.Fatal("expected missing HOST error")
	}
}

func TestBuildCFManifestFanOut(t *testing.T) {
	src := CFSource{
		ID: "abcd1234ef", Name: "sg", Host: "edgetunnel-full.wbi35772.workers.dev",
		UUID: "9679f5cf-6da5-4f2b-9f3e-39278492a4e2", Port: 443,
		PathTemplate: "/proxyip=<ip>",
		Egress:       []string{"1.2.3.4:443", "5.6.7.8:443"},
		Edge:         []string{"104.18.33.247", "104.17.25.215"},
	}
	if err := normalizeCFSource(&src); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	manifest, err := buildCFManifest(src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(manifest.Proxies) != 2 {
		t.Fatalf("proxies = %d", len(manifest.Proxies))
	}
	first := manifest.Proxies[0]
	if first.WSOpts.Path != "/proxyip=1.2.3.4:443" {
		t.Fatalf("path = %q", first.WSOpts.Path)
	}
	if first.Server != "104.18.33.247" || first.Port != 443 || !first.TLS {
		t.Fatalf("proxy = %+v", first)
	}
	if manifest.Proxies[1].Server != "104.17.25.215" {
		t.Fatalf("edge rotation failed: %+v", manifest.Proxies[1])
	}
	if first.WSOpts.Headers.Host != "edgetunnel-full.wbi35772.workers.dev" {
		t.Fatalf("host header = %q", first.WSOpts.Headers.Host)
	}
	raw, err := json.Marshal(manifest)
	if err != nil || !strings.Contains(string(raw), `"proxies"`) {
		t.Fatalf("manifest json: %v %s", err, raw)
	}
}

func TestBuildCFManifestSingleNode(t *testing.T) {
	src := CFSource{
		ID: "abcd1234ef", Name: "direct", Host: "a.example.workers.dev",
		UUID: "9679f5cf-6da5-4f2b-9f3e-39278492a4e2", PathTemplate: "/",
	}
	if err := normalizeCFSource(&src); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	manifest, err := buildCFManifest(src)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(manifest.Proxies) != 1 || manifest.Proxies[0].WSOpts.Path != "/" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestNormalizeCFSourceRejectsBadInput(t *testing.T) {
	cases := []CFSource{
		{Name: "", Host: "a.example.workers.dev", UUID: "9679f5cf-6da5-4f2b-9f3e-39278492a4e2"},
		{Name: "x", Host: "bad host", UUID: "9679f5cf-6da5-4f2b-9f3e-39278492a4e2"},
		{Name: "x", Host: "a.example.workers.dev", UUID: "nope"},
		{Name: "x", Host: "a.example.workers.dev", UUID: "9679f5cf-6da5-4f2b-9f3e-39278492a4e2", PathTemplate: "/no-placeholder", Egress: []string{"1.2.3.4:443"}},
	}
	for i := range cases {
		if err := normalizeCFSource(&cases[i]); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
