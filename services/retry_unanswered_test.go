package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A third pod, listed but never served: dials to it are refused.
const podC = "10.233.62.3"

// dropBeforeHeaders takes the request and drops the connection after
// writing sent: reset (the pod's forced close at the end of its drain) or
// closed.
func dropBeforeHeaders(p *retryPod, reset bool, sent string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte(sent))
		if reset {
			_ = conn.(*net.TCPConn).SetLinger(0)
		}
		_ = conn.Close()
	}
}

// countDials counts the transport's dials, refused ones included.
func (g *retryRig) countDials() *atomic.Int32 {
	var n atomic.Int32
	dial := g.tr.DialContext
	g.tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		n.Add(1)
		return dial(ctx, network, addr)
	}
	return &n
}

// roundTrip sends req to podA through retryTransport.
func (g *retryRig) roundTrip(req *http.Request, delay time.Duration) (*http.Response, error) {
	req = WithRetryContext(req, &RetryContext{
		Src:               &Source{Type: "default", InfoHash: retryHash},
		SvcLoc:            g.sl,
		Cfg:               &g.cfg,
		Transport:         g.tr,
		ExternalTransport: g.tr,
		MaxRetries:        3,
		RetryDelay:        delay,
	})
	return (&retryTransport{&redirectFollowingTransport{g.tr, g.tr}}).RoundTrip(req)
}

func podARequest(t *testing.T, ctx context.Context, method string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+podA+":8080/file", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=100-")
	req.Header.Set("X-Uid", harnessUid)
	return req
}

// A GET or HEAD that podA did not answer at all goes once to the pod the
// torrent resolves to without podA, with the client's Range and the
// token's uid, and the client gets that pod's answer.
func TestRetryUnansweredGoesToAnotherPod(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		// a: podA's handler; nil: podA refuses connections.
		a     func(*retryPod) http.HandlerFunc
		wantA int32
	}{
		{"refused", http.MethodGet, nil, 0},
		{"HEAD refused", http.MethodHead, nil, 0},
		{"reset before headers", http.MethodGet, func(p *retryPod) http.HandlerFunc { return dropBeforeHeaders(p, true, "") }, 1},
		{"closed before headers", http.MethodGet, func(p *retryPod) http.HandlerFunc { return dropBeforeHeaders(p, false, "") }, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			a := g.pods[podA]
			if c.a == nil {
				a.down.Store(true)
				g.serve(podA, seederHandler(a, false, nil))
			} else {
				g.serve(podA, c.a(a))
			}
			b := g.pods[podB]
			var rangeB, uidB atomic.Value
			seeder := seederHandler(b, false, nil)
			g.serve(podB, func(w http.ResponseWriter, r *http.Request) {
				rangeB.Store(r.Header.Get("Range"))
				uidB.Store(r.Header.Get("X-Uid"))
				seeder(w, r)
			})
			resp, err := g.roundTrip(podARequest(t, context.Background(), c.method, nil), 0)
			if err != nil {
				t.Fatalf("client got %v, want podB's answer", err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			want := retryPayload()[100:]
			if c.method == http.MethodHead {
				want = nil
			}
			if resp.StatusCode != http.StatusPartialContent || err != nil || !bytes.Equal(got, want) {
				t.Fatalf("client got %d, %d bytes (err %v); want 206 with %d bytes", resp.StatusCode, len(got), err, len(want))
			}
			if ha, hb := g.hits(); ha != c.wantA || hb != 1 {
				t.Fatalf("requests: podA %d, podB %d; want %d and 1", ha, hb, c.wantA)
			}
			if r, _ := rangeB.Load().(string); r != "bytes=100-" {
				t.Errorf("podB got Range %q, want the client's", r)
			}
			if u, _ := uidB.Load().(string); u != harnessUid {
				t.Errorf("podB got X-Uid %q, want the token's", u)
			}
		})
	}
}

// Once a byte of a response came back, the request is not sent again: a
// 1xx went to the client already, and a pod that began to answer may
// have begun to act on it.
func TestRetryUnansweredNotOnceAResponseByteCame(t *testing.T) {
	for _, c := range []struct{ name, sent string }{
		{"status line, then closed", "HTTP/1.1 200 OK\r\n"},
		{"103, then closed", "HTTP/1.1 103 Early Hints\r\nLink: </a>; rel=preload\r\n\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			g.serve(podA, dropBeforeHeaders(g.pods[podA], false, c.sent))
			g.serve(podB, seederHandler(g.pods[podB], false, nil))
			if resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), 0); err == nil {
				_ = resp.Body.Close()
				t.Fatalf("client got %d, want podA's error", resp.StatusCode)
			}
			if ha, hb := g.hits(); ha != 1 || hb != 0 {
				t.Fatalf("requests: podA %d, podB %d; want 1 and 0", ha, hb)
			}
		})
	}
}

// Nothing but a GET or HEAD without a body is sent again.
func TestRetryUnansweredOnlyReplayable(t *testing.T) {
	for _, c := range []struct {
		name, method string
		body         io.Reader
	}{
		{"POST", http.MethodPost, nil},
		{"GET with a body", http.MethodGet, strings.NewReader("x")},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			g.pods[podA].down.Store(true)
			g.serve(podA, seederHandler(g.pods[podA], false, nil))
			g.serve(podB, seederHandler(g.pods[podB], false, nil))
			if resp, err := g.roundTrip(podARequest(t, context.Background(), c.method, c.body), 0); err == nil {
				_ = resp.Body.Close()
				t.Fatalf("client got %d, want the refusal", resp.StatusCode)
			}
			if _, hb := g.hits(); hb != 0 {
				t.Fatalf("podB got %d requests", hb)
			}
		})
	}
}

// One attempt more, whatever it meets, even with a pod left untried; none
// when no pod is left.
func TestRetryUnansweredOnce(t *testing.T) {
	for _, c := range []struct {
		name      string
		listed    []string
		wantDials int32
	}{
		{"every pod refuses", []string{podA, podB, podC}, 2},
		{"no other pod", []string{podA}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, c.listed)
			for _, p := range g.pods {
				p.down.Store(true)
			}
			dials := g.countDials()
			// Bounded, so that a retry that loops fails here, not at the
			// suite's timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := g.roundTrip(podARequest(t, ctx, http.MethodGet, nil), 0)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("client got %d, want the refusal", resp.StatusCode)
			}
			if !isDialError(err) {
				t.Errorf("client got %v, want a refusal", err)
			}
			if n := dials.Load(); n != c.wantDials {
				t.Fatalf("%d dials, want %d", n, c.wantDials)
			}
		})
	}
}

// A client that leaves during the delay ends the request at once: as its
// own leaving (context.Canceled), with no attempt made.
func TestRetryUnansweredStopsWhenTheClientLeaves(t *testing.T) {
	g := newRetryRig(t, []string{podA, podB})
	g.pods[podA].down.Store(true)
	g.serve(podA, seederHandler(g.pods[podA], false, nil))
	g.serve(podB, seederHandler(g.pods[podB], false, nil))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	resp, err := g.roundTrip(podARequest(t, ctx, http.MethodGet, nil), 5*time.Second)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("client got %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v, want right after the client left", d)
	}
	if _, hb := g.hits(); hb != 0 {
		t.Fatalf("podB got %d requests after the client left", hb)
	}
}
