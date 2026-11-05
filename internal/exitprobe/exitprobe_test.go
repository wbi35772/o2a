package exitprobe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// socks5Server is a minimal SOCKS5 CONNECT proxy for tests. It forwards every
// connection to the requested address, which lets a probe reach the httptest
// servers below without any external network.
func socks5Server(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-done }
}

func handleSOCKS5(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return
		}
		buf := make([]byte, int(size[0]))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = string(buf)
	case 4:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(portBuf))
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	go func() { _, _ = io.Copy(target, conn) }()
	_, _ = io.Copy(conn, target)
}

func dialThrough(proxyAddr string) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, err
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
			conn.Close()
			return nil, err
		}
		ack := make([]byte, 2)
		if _, err := io.ReadFull(conn, ack); err != nil {
			conn.Close()
			return nil, err
		}
		buf := []byte{5, 1, 0, 3, byte(len(host))}
		buf = append(buf, host...)
		buf = binary.BigEndian.AppendUint16(buf, uint16(port))
		if _, err := conn.Write(buf); err != nil {
			conn.Close()
			return nil, err
		}
		reply := make([]byte, 10)
		if _, err := io.ReadFull(conn, reply); err != nil {
			conn.Close()
			return nil, err
		}
		if reply[1] != 0 {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect refused: %d", reply[1])
		}
		return conn, nil
	}
}

func TestProbeOK(t *testing.T) {
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "opencode/latest/2.1.1/cli" {
			t.Errorf("unexpected user agent %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer public" {
			t.Errorf("unexpected authorization %q", got)
		}
		if got := r.Header.Get("x-opencode-project"); len(got) != 40 {
			t.Errorf("unexpected project header %q", got)
		}
		if r.Header.Get("x-opencode-request") != "" {
			t.Error("v2 lane must not send x-opencode-request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer inference.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.9")
	}))
	defer ipSrv.Close()

	proxy, stop := socks5Server(t)
	defer stop()
	res := Probe(context.Background(), dialThrough(proxy), Options{
		Model: "beam-free", Timeout: 5 * time.Second,
		InferenceURL: inference.URL, IPURL: ipSrv.URL, FallbackURL: ipSrv.URL,
	})
	if res.Quota != QuotaOK {
		t.Fatalf("quota = %q (%s)", res.Quota, res.Note)
	}
	if res.EgressIP != "203.0.113.9" {
		t.Fatalf("egress = %q", res.EgressIP)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestProbeLimited(t *testing.T) {
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "48982")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`)
	}))
	defer inference.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "198.51.100.7")
	}))
	defer ipSrv.Close()

	proxy, stop := socks5Server(t)
	defer stop()
	res := Probe(context.Background(), dialThrough(proxy), Options{
		Model: "beam-free", Timeout: 5 * time.Second,
		InferenceURL: inference.URL, IPURL: ipSrv.URL, FallbackURL: ipSrv.URL,
	})
	if res.Quota != QuotaLimited {
		t.Fatalf("quota = %q (%s)", res.Quota, res.Note)
	}
	if res.RetryAfterSeconds != 48982 {
		t.Fatalf("retry after = %d", res.RetryAfterSeconds)
	}
}

func TestProbeBlocked(t *testing.T) {
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Free models aren't available from your current network due to abuse of traffic"}}`)
	}))
	defer inference.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "198.51.100.8")
	}))
	defer ipSrv.Close()

	proxy, stop := socks5Server(t)
	defer stop()
	res := Probe(context.Background(), dialThrough(proxy), Options{
		Model: "beam-free", Timeout: 5 * time.Second,
		InferenceURL: inference.URL, IPURL: ipSrv.URL, FallbackURL: ipSrv.URL,
	})
	if res.Quota != QuotaBlocked {
		t.Fatalf("quota = %q (%s)", res.Quota, res.Note)
	}
}

func TestProbeNoDialer(t *testing.T) {
	res := Probe(context.Background(), nil, Options{})
	if res.Quota != QuotaError || res.Note == "" {
		t.Fatalf("unexpected result %+v", res)
	}
}
