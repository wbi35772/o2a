// Package edgescan finds Cloudflare edge addresses that actually reach one
// deployed Worker. It samples Cloudflare's published IPv4 ranges, completes a
// TLS handshake with the Worker host as SNI, and validates the Worker's
// /version endpoint before keeping the address. The result is an entry-point
// list for CFSource.Edge, not an egress pool: the egress IP is still chosen by
// the Worker's ws path (proxyip/socks5).
package edgescan

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cloudflareIPv4 is the published Cloudflare IPv4 list. Sampling these ranges
// gives a spread of anycast entry points instead of one hard-coded set.
var cloudflareIPv4 = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
}

type block struct {
	base uint32
	size uint32
}

var blocks = mustBlocks(cloudflareIPv4)

func mustBlocks(cidrs []string) []block {
	out := make([]block, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("edgescan: bad built-in CIDR " + cidr)
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			panic("edgescan: not IPv4 " + cidr)
		}
		ones, bits := ipnet.Mask.Size()
		out = append(out, block{
			base: binary.BigEndian.Uint32(ip4),
			size: uint32(1) << (bits - ones),
		})
	}
	return out
}

// Options controls one scan.
type Options struct {
	Host        string
	Port        int
	UUID        string
	Candidates  int
	Keep        int
	Timeout     time.Duration
	Concurrency int
}

// Hit is one validated edge address.
type Hit struct {
	IP        string `json:"ip"`
	LatencyMS int    `json:"latency_ms"`
}

// Report is the outcome of a scan.
type Report struct {
	Attempted  int   `json:"attempted"`
	Reachable  int   `json:"reachable"`
	DurationMS int   `json:"duration_ms"`
	Hits       []Hit `json:"hits"`
}

func (o *Options) applyDefaults() {
	if o.Port == 0 {
		o.Port = 443
	}
	if o.Candidates <= 0 {
		o.Candidates = 200
	}
	if o.Keep <= 0 {
		o.Keep = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 1500 * time.Millisecond
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 32
	}
}

// Scan probes up to opts.Candidates sampled addresses and returns the fastest
// opts.Keep validated ones. Addresses are deduplicated per /24 so a single
// anycast cluster cannot fill the list.
func Scan(ctx context.Context, opts Options) (Report, error) {
	opts.applyDefaults()
	if strings.TrimSpace(opts.Host) == "" || strings.TrimSpace(opts.UUID) == "" {
		return Report{}, errors.New("worker host and uuid are required")
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return Report{}, errors.New("worker port is invalid")
	}
	if opts.Candidates > 1000 {
		return Report{}, errors.New("candidates must be <= 1000")
	}
	if opts.Keep > 32 {
		return Report{}, errors.New("keep must be <= 32")
	}
	started := time.Now()
	ips := sampleIPs(opts.Candidates, rand.New(rand.NewSource(time.Now().UnixNano())))

	jobs := make(chan string)
	var mu sync.Mutex
	hits := make([]Hit, 0, opts.Keep)
	attempted, reachable := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				if ctx.Err() != nil {
					return
				}
				hit, ok := probe(ctx, opts, ip)
				mu.Lock()
				attempted++
				if ok {
					reachable++
					hits = append(hits, hit)
				}
				mu.Unlock()
			}
		}()
	}
	for _, ip := range ips {
		select {
		case jobs <- ip:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].LatencyMS == hits[j].LatencyMS {
			return hits[i].IP < hits[j].IP
		}
		return hits[i].LatencyMS < hits[j].LatencyMS
	})
	if len(hits) > opts.Keep {
		hits = hits[:opts.Keep]
	}
	return Report{
		Attempted:  attempted,
		Reachable:  reachable,
		DurationMS: int(time.Since(started).Milliseconds()),
		Hits:       hits,
	}, nil
}

// probe validates one entry address by fetching the Worker version endpoint
// through it. The TLS SNI and HTTP Host both carry the Worker host, so success
// means this entry can serve the deployed Worker.
func probe(ctx context.Context, opts Options, ip string) (Hit, bool) {
	started := time.Now()
	deadline := started.Add(opts.Timeout)
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(opts.Port)))
	if err != nil {
		return Hit{}, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	host := strings.TrimSpace(opts.Host)
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return Hit{}, false
	}
	path := "/version?uuid=" + url.QueryEscape(opts.UUID)
	request := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: opencode2api-edge-scan\r\nAccept: */*\r\nConnection: close\r\n\r\n",
		path, host,
	)
	if _, err := io.WriteString(tlsConn, request); err != nil {
		return Hit{}, false
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(tlsConn, 8<<10)), &http.Request{Method: http.MethodGet})
	if err != nil {
		return Hit{}, false
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<10))
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Version")) {
		return Hit{}, false
	}
	return Hit{IP: ip, LatencyMS: int(time.Since(started).Milliseconds())}, true
}

// sampleIPs draws unique addresses from the Cloudflare ranges and keeps at most
// one address per /24.
func sampleIPs(count int, rng *rand.Rand) []string {
	if count <= 0 {
		return nil
	}
	seen := make(map[string]struct{}, count)
	out := make([]string, 0, count)
	for attempts := 0; len(out) < count && attempts < count*80; attempts++ {
		block := blocks[rng.Intn(len(blocks))]
		value := block.base + uint32(rng.Int63n(int64(block.size)))
		ip := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
		key := fmt.Sprintf("%d.%d.%d", byte(value>>24), byte(value>>16), byte(value>>8))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip.String())
	}
	return out
}
