//go:build linux && blackhole

// Upstreams that vanish without a FIN or RST, as a pod does whose network
// goes before its kernel has sent the FIN: iptables drops a test server's
// port on lo both ways, so neither kernel hears from the other again. Needs
// NET_ADMIN; run with testdata/blackhole.sh (a golang container).
//
// The bounds are for the flags' defaults: keepalive 5 s, TCP_USER_TIMEOUT
// 20 s. With Go's default dialer the same reads wait 150 s (keepalive) or
// tcp_retries2, ~15 min (unacknowledged data); bhBlocked only shows they
// are still waiting well past the new bound.

package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
)

const (
	bhBound   = 25 * time.Second
	bhBlocked = 35 * time.Second
)

// bhServer serves each connection on 127.0.0.1 with serve; its port.
func bhServer(t *testing.T, serve func(net.Conn)) (addr, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go serve(c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return ln.Addr().String(), port
}

// blackhole drops all TCP to and from port until the test ends.
func blackhole(t *testing.T, port string) {
	t.Helper()
	rules := [][]string{
		{"INPUT", "-p", "tcp", "--dport", port, "-j", "DROP"},
		{"INPUT", "-p", "tcp", "--sport", port, "-j", "DROP"},
	}
	for _, r := range rules {
		if out, err := exec.Command("iptables", append([]string{"-w", "-I"}, r...)...).CombinedOutput(); err != nil {
			t.Fatalf("iptables: %v: %s", err, out)
		}
	}
	t.Cleanup(func() {
		for _, r := range rules {
			_ = exec.Command("iptables", append([]string{"-w", "-D"}, r...)...).Run()
		}
	})
}

// respond reads the request and writes a 200 of size bytes, written by body.
func respond(c net.Conn, size int64, body func(io.Writer) error) {
	if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
		return
	}
	if _, err := fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", size); err != nil {
		return
	}
	_ = body(c)
}

func bhGet(t *testing.T, tr *http.Transport, addr string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// waitErr runs read and checks how it ends: with ETIMEDOUT within bhBound
// when noticed, still blocked at bhBlocked otherwise.
func waitErr(t *testing.T, noticed bool, read func() error) {
	t.Helper()
	start := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- read() }()
	select {
	case err := <-errc:
		took := time.Since(start)
		if !noticed {
			t.Fatalf("ended after %v (%v); want still blocked at %v", took, err, bhBlocked)
		}
		if took > bhBound || !errors.Is(err, syscall.ETIMEDOUT) || !isRetryableError(err) {
			t.Fatalf("after %v: %v; want a retryable ETIMEDOUT within %v", took, err, bhBound)
		}
		t.Logf("noticed after %v: %v", took.Round(100*time.Millisecond), err)
	case <-time.After(bhBlocked):
		if noticed {
			t.Fatalf("still blocked at %v", bhBlocked)
		}
		t.Logf("still blocked at %v", bhBlocked)
	}
}

var bhBoth0 = []string{upstreamKeepAliveFlag, "0s", upstreamTCPUserTimeoutFlag, "0s"}

// 2026-10-07: a stream from a pod deleted mid-transfer. thp only reads, so
// it is keepalive that notices.
func TestBlackholeMidStream(t *testing.T) {
	for _, c := range []struct {
		name     string
		kv       []string
		external bool
		noticed  bool
	}{
		{"transport", nil, false, true},
		{"externalTransport", nil, true, true},
		// The probe count alone (3) bounds it the same where TCP_USER_TIMEOUT,
		// which takes its place on Linux, is off.
		{"transport, TCP_USER_TIMEOUT 0", []string{upstreamTCPUserTimeoutFlag, "0s"}, false, true},
		{"transport, both flags 0 (Go's default dialer)", bhBoth0, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			addr, port := bhServer(t, func(c net.Conn) {
				respond(c, 1<<40, func(w io.Writer) error {
					buf := make([]byte, 32<<10)
					for {
						if _, err := w.Write(buf); err != nil {
							return err
						}
						time.Sleep(5 * time.Millisecond)
					}
				})
			})
			p := testHTTPProxy(t, c.kv...)
			tr := p.transport
			if c.external {
				tr = p.externalTransport
			}
			resp := bhGet(t, tr, addr)
			if _, err := io.CopyN(io.Discard, resp.Body, 1<<20); err != nil {
				t.Fatal(err)
			}
			blackhole(t, port)
			waitErr(t, c.noticed, func() error {
				_, err := io.Copy(io.Discard, resp.Body)
				return err
			})
		})
	}
}

// A request written to a peer that has just vanished: its bytes are never
// acknowledged, and keepalive does not probe while they are in flight.
// TCP_USER_TIMEOUT is what notices.
func TestBlackholeUnackedRequest(t *testing.T) {
	for _, c := range []struct {
		name    string
		kv      []string
		noticed bool
	}{
		{"defaults", nil, true},
		{"keepalive only (TCP_USER_TIMEOUT 0)", []string{upstreamTCPUserTimeoutFlag, "0s"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			addr, port := bhServer(t, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
			conn, err := testHTTPProxy(t, c.kv...).transport.DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			blackhole(t, port)
			if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: upstream\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			waitErr(t, c.noticed, func() error {
				_, err := conn.Read(make([]byte, 1))
				return err
			})
		})
	}
}

// No false alarm: an upstream that is alive answers the probes from its
// kernel, whether its process sends nothing for a minute (a seeder waiting
// on its swarm) or thp stops reading and closes its window (a slow client,
// the limiter).
func TestSilentUpstreamNotCut(t *testing.T) {
	const pause = 60 * time.Second
	t.Run("upstream silent", func(t *testing.T) {
		t.Parallel()
		const chunk = 64 << 10
		addr, _ := bhServer(t, func(c net.Conn) {
			respond(c, 2*chunk, func(w io.Writer) error {
				if _, err := w.Write(make([]byte, chunk)); err != nil {
					return err
				}
				time.Sleep(pause)
				_, err := w.Write(make([]byte, chunk))
				return err
			})
		})
		resp := bhGet(t, testHTTPProxy(t).transport, addr)
		start := time.Now()
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil || n != 2*chunk {
			t.Fatalf("read %d of %d after %v: %v", n, 2*chunk, time.Since(start), err)
		}
		if took := time.Since(start); took < pause {
			t.Fatalf("done in %v: the upstream never went silent for %v", took, pause)
		}
	})
	t.Run("thp not reading", func(t *testing.T) {
		t.Parallel()
		const total = 64 << 20
		var written atomic.Int64
		addr, _ := bhServer(t, func(c net.Conn) {
			respond(c, total, func(w io.Writer) error {
				buf := make([]byte, 32<<10)
				for written.Load() < total {
					n, err := w.Write(buf)
					written.Add(int64(n))
					if err != nil {
						return err
					}
				}
				return nil
			})
		})
		resp := bhGet(t, testHTTPProxy(t).transport, addr)
		if _, err := io.CopyN(io.Discard, resp.Body, 1<<20); err != nil {
			t.Fatal(err)
		}
		time.Sleep(pause)
		if w := written.Load(); w >= total {
			t.Fatalf("the upstream wrote all %d bytes while thp did not read: its window never closed", w)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil || n != total-1<<20 {
			t.Fatalf("read %d of %d after the pause: %v", n, total-1<<20, err)
		}
	})
}
