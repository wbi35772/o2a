// socksmux exposes one platform-routed SOCKS5 port and fans it out to N
// local wireproxy instances by username: warpuser01 -> backend 1, etc.
// The same listener also serves the pool management API and web UI over HTTP.
package main

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type backendStats struct {
	conns atomic.Uint64
	up    atomic.Uint64
	down  atomic.Uint64
}

type mux struct {
	listen      string
	backendHost string
	basePort    int
	count       int
	upUser      string
	upPass      string
	clientPass  string
	stats       []*backendStats
	api         *apiServer
}

// bufferedConn keeps the bytes peeked from the connection while handing the
// rest of the stream to the SOCKS handler or the HTTP server.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func main() {
	var m mux
	var apiToken, stateDir, publicHost, privateHost, poolID string
	var rotation, batch, health, minHealthy, muxPort int
	flag.StringVar(&m.listen, "listen", "0.0.0.0:1081", "listen address")
	flag.StringVar(&m.backendHost, "backend-host", "127.0.0.1", "wireproxy host")
	flag.IntVar(&m.basePort, "base-port", 1082, "first backend port")
	flag.IntVar(&m.count, "count", 16, "number of backends")
	flag.StringVar(&m.upUser, "upstream-user", "warpuser", "upstream SOCKS username")
	flag.StringVar(&m.upPass, "upstream-pass", "", "upstream SOCKS password")
	flag.StringVar(&m.clientPass, "client-pass", "", "password clients must present")
	flag.StringVar(&apiToken, "api-token", "", "management API bearer token (empty disables the API)")
	flag.StringVar(&stateDir, "state-dir", "/data", "pool state directory")
	flag.StringVar(&publicHost, "public-host", "", "public host advertised to clients")
	flag.StringVar(&privateHost, "private-host", "", "private host advertised to clients")
	flag.StringVar(&poolID, "pool-id", "", "pool identifier shown in the API")
	flag.IntVar(&rotation, "rotation-interval", 900, "default node rotation interval seconds")
	flag.IntVar(&batch, "reroll-batch", 4, "default nodes rerolled per cycle")
	flag.IntVar(&health, "health-interval", 300, "default health probe interval seconds")
	flag.IntVar(&minHealthy, "min-healthy", 14, "default minimum healthy nodes")
	flag.IntVar(&muxPort, "mux-port", 0, "advertised mux port (default: listen port)")
	flag.Parse()

	if m.upPass == "" || m.clientPass == "" {
		log.Fatal("upstream-pass and client-pass are required")
	}
	if m.count <= 0 {
		log.Fatal("count must be positive")
	}
	if muxPort == 0 {
		if _, port, err := net.SplitHostPort(m.listen); err == nil {
			muxPort, _ = strconv.Atoi(port)
		}
	}
	if muxPort == 0 {
		muxPort = 1081
	}
	m.stats = make([]*backendStats, m.count)
	for i := range m.stats {
		m.stats[i] = &backendStats{}
	}

	ln, err := net.Listen("tcp", m.listen)
	if err != nil {
		log.Fatalf("listen %s: %v", m.listen, err)
	}
	if apiToken != "" {
		m.api = newAPIServer(&m, apiOptions{
			StateDir:    stateDir,
			Token:       apiToken,
			PoolID:      poolID,
			PublicHost:  publicHost,
			PrivateHost: privateHost,
			MuxPort:     muxPort,
			SocksUser:   m.upUser,
			Rotation:    rotation,
			Batch:       batch,
			Health:      health,
			MinHealthy:  minHealthy,
		})
		go m.api.serve()
		log.Printf("management API enabled on %s (state dir %s)", m.listen, stateDir)
	}
	log.Printf("socks mux %s -> %s:%d..%d (%d backends, users %s01..%s%02d)",
		m.listen, m.backendHost, m.basePort, m.basePort+m.count-1, m.count,
		m.upUser, m.upUser, m.count)
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			log.Printf("accept: %v", err)
			continue
		}
		bc := &bufferedConn{Conn: conn, r: bufio.NewReader(conn)}
		first, err := bc.r.Peek(1)
		if err != nil {
			_ = conn.Close()
			continue
		}
		if first[0] == 0x05 {
			go m.handle(bc)
			continue
		}
		if m.api != nil {
			m.api.dispatch(bc)
		} else {
			_ = conn.Close()
		}
	}
}

func (m *mux) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	index, err := m.clientAuth(client)
	if err != nil {
		return
	}
	stats := m.stats[index]
	stats.conns.Add(1)

	addr := net.JoinHostPort(m.backendHost, strconv.Itoa(m.basePort+index))
	upstream, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("backend %s: %v", addr, err)
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Second))

	if err := m.upstreamAuth(upstream); err != nil {
		log.Printf("backend %s auth: %v", addr, err)
		return
	}

	_ = client.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	relay(client, upstream, stats)
}

// clientAuth performs the client-facing SOCKS5 greeting and username/password
// exchange, returning the zero-based backend index selected by the username.
func (m *mux) clientAuth(c net.Conn) (int, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c, header); err != nil {
		return 0, err
	}
	if header[0] != 5 {
		return 0, fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return 0, err
	}
	offersUserPass := false
	for _, method := range methods {
		if method == 0x02 {
			offersUserPass = true
		}
	}
	if !offersUserPass {
		_, _ = c.Write([]byte{5, 0xff})
		return 0, errors.New("client does not offer username/password auth")
	}
	if _, err := c.Write([]byte{5, 0x02}); err != nil {
		return 0, err
	}

	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return 0, err
	}
	if head[0] != 1 {
		return 0, fmt.Errorf("unsupported auth version %d", head[0])
	}
	user := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, user); err != nil {
		return 0, err
	}
	five := make([]byte, 1)[0:1]
	if _, err := io.ReadFull(c, five); err != nil {
		return 0, err
	}
	pass := make([]byte, int(five[0]))
	if _, err := io.ReadFull(c, pass); err != nil {
		return 0, err
	}

	if subtle.ConstantTimeCompare(pass, []byte(m.clientPass)) != 1 {
		_, _ = c.Write([]byte{1, 1})
		return 0, errors.New("wrong password")
	}
	index, ok := m.backendFor(string(user))
	if !ok {
		_, _ = c.Write([]byte{1, 1})
		return 0, fmt.Errorf("unknown user %q", string(user))
	}
	_, _ = c.Write([]byte{1, 0})
	return index, nil
}

func (m *mux) backendFor(user string) (int, bool) {
	if user == m.upUser {
		return 0, true
	}
	if !strings.HasPrefix(user, m.upUser) {
		return 0, false
	}
	suffix := strings.TrimPrefix(user, m.upUser)
	if suffix == "" {
		return 0, true
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || n > m.count {
		return 0, false
	}
	return n - 1, true
}

// upstreamAuth authenticates this mux to a local wireproxy instance.
func (m *mux) upstreamAuth(c net.Conn) error {
	if _, err := c.Write([]byte{5, 1, 0x02}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	if resp[0] != 5 || resp[1] != 0x02 {
		return fmt.Errorf("auth method rejected (%v)", resp)
	}
	buf := make([]byte, 0, 3+len(m.upUser)+len(m.upPass))
	buf = append(buf, 1, byte(len(m.upUser)))
	buf = append(buf, m.upUser...)
	buf = append(buf, byte(len(m.upPass)))
	buf = append(buf, m.upPass...)
	if _, err := c.Write(buf); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, resp); err != nil {
		return err
	}
	if resp[0] != 1 || resp[1] != 0 {
		return errors.New("credentials rejected")
	}
	return nil
}

// relay copies bytes in both directions and records per-backend traffic.
func relay(a, b net.Conn, s *backendStats) {
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(a, b)
		s.down.Add(uint64(n))
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(b, a)
		s.up.Add(uint64(n))
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
