package edgescan

import (
	"context"
	"math/rand"
	"net"
	"testing"
	"time"
)

func TestSampleIPsAreUniquePer24AndInRanges(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	ips := sampleIPs(128, rng)
	if len(ips) != 128 {
		t.Fatalf("got %d addresses, want 128", len(ips))
	}
	seen := make(map[string]bool, len(ips))
	for _, text := range ips {
		ip := net.ParseIP(text).To4()
		if ip == nil {
			t.Fatalf("%q is not IPv4", text)
		}
		value := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		inside := false
		for _, block := range blocks {
			if value >= block.base && value < block.base+block.size {
				inside = true
				break
			}
		}
		if !inside {
			t.Fatalf("%s is outside the published Cloudflare ranges", text)
		}
		key := net.IPv4(ip[0], ip[1], ip[2], 0).String()
		if seen[key] {
			t.Fatalf("duplicate /24 %s", key)
		}
		seen[key] = true
	}
}

func TestScanRejectsIncompleteOptions(t *testing.T) {
	if _, err := Scan(context.Background(), Options{UUID: "u"}); err == nil {
		t.Fatal("missing host must fail")
	}
	if _, err := Scan(context.Background(), Options{Host: "example.com"}); err == nil {
		t.Fatal("missing uuid must fail")
	}
	if _, err := Scan(context.Background(), Options{Host: "example.com", UUID: "u", Candidates: 5000}); err == nil {
		t.Fatal("oversized candidate count must fail")
	}
}

func TestOptionsDefaults(t *testing.T) {
	options := Options{}
	options.applyDefaults()
	if options.Port != 443 || options.Candidates != 200 || options.Keep != 8 ||
		options.Timeout != 1500*time.Millisecond || options.Concurrency != 32 {
		t.Fatalf("unexpected defaults: %+v", options)
	}
}
