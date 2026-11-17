package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// unanswered reads promRetryUnanswered by outcome.
func unanswered(t *testing.T) map[string]float64 {
	t.Helper()
	m := map[string]float64{}
	for _, o := range []string{"success", "failure", "no_pod", "client_gone"} {
		m[o] = counterValue(t, promRetryUnanswered.WithLabelValues(o))
	}
	return m
}

// wantUnanswered fails t unless, since before, outcome alone went up, by
// one; nothing at all for "".
func wantUnanswered(t *testing.T, before map[string]float64, outcome string) {
	t.Helper()
	for o, n := range unanswered(t) {
		want := before[o]
		if o == outcome {
			want++
		}
		if n != want {
			t.Errorf("retry_unanswered_total{outcome=%q} went from %v to %v, want %v", o, before[o], n, want)
		}
	}
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
			before := unanswered(t)
			resp, err := g.roundTrip(podARequest(t, context.Background(), c.method, nil), 0)
			if err != nil {
				t.Fatalf("client got %v, want podB's answer", err)
			}
			wantUnanswered(t, before, "success")
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
// have begun to act on it. Nor does the pod go on the shared ignore list
// when the target it redirects to refuses: it answered.
func TestRetryUnansweredNotOnceAResponseByteCame(t *testing.T) {
	for _, c := range []struct{ name, sent string }{
		{"status line, then closed", "HTTP/1.1 200 OK\r\n"},
		{"103, then closed", "HTTP/1.1 103 Early Hints\r\nLink: </a>; rel=preload\r\n\r\n"},
		{"redirect to a target that refuses", "HTTP/1.1 302 Found\r\nLocation: http://10.233.99.9:9000/file\r\nContent-Length: 0\r\n\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			g.serve(podA, dropBeforeHeaders(g.pods[podA], false, c.sent))
			g.serve(podB, seederHandler(g.pods[podB], false, nil))
			before := unanswered(t)
			if resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), 0); err == nil {
				_ = resp.Body.Close()
				t.Fatalf("client got %d, want podA's error", resp.StatusCode)
			}
			wantUnanswered(t, before, "")
			if ha, hb := g.hits(); ha != 1 || hb != 0 {
				t.Fatalf("requests: podA %d, podB %d; want 1 and 0", ha, hb)
			}
			if g.sl.ignore.IsIgnored(podA) {
				t.Fatal("podA answered and is on the shared ignore list")
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

// One attempt more, whatever it meets, even with a pod left untried, at
// once; with no pod left, none, after the delay and a second pick.
func TestRetryUnansweredOnce(t *testing.T) {
	const delay = 300 * time.Millisecond
	for _, c := range []struct {
		name      string
		listed    []string
		wantDials int32
		outcome   string
		waits     bool
	}{
		{"every pod refuses", []string{podA, podB, podC}, 2, "failure", false},
		{"no other pod", []string{podA}, 1, "no_pod", true},
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
			before := unanswered(t)
			start := time.Now()
			resp, err := g.roundTrip(podARequest(t, ctx, http.MethodGet, nil), delay)
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
			if d := time.Since(start); (d >= delay) != c.waits {
				t.Errorf("returned after %v; waited the %v delay: %v, want %v", d, delay, d >= delay, c.waits)
			}
			wantUnanswered(t, before, c.outcome)
		})
	}
}

// The pause comes only when no other pod is there to take the request: with
// one, it goes at once; with none (an Environment location is one host),
// after the delay, to the host that is back by then.
func TestRetryUnansweredWaitsOnlyWithoutAnotherPod(t *testing.T) {
	const delay = 300 * time.Millisecond
	t.Run("another pod: at once", func(t *testing.T) {
		g := newRetryRig(t, []string{podA, podB})
		g.pods[podA].down.Store(true)
		g.serve(podA, seederHandler(g.pods[podA], false, nil))
		g.serve(podB, seederHandler(g.pods[podB], false, nil))
		start := time.Now()
		resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), 5*time.Second)
		if err != nil {
			t.Fatalf("client got %v, want podB's answer", err)
		}
		_ = resp.Body.Close()
		if d := time.Since(start); d > time.Second {
			t.Fatalf("podB answered after %v, want at once, not after the 5 s delay", d)
		}
		if _, hb := g.hits(); hb != 1 {
			t.Fatalf("podB got %d requests, want 1", hb)
		}
	})
	t.Run("same host: after the delay", func(t *testing.T) {
		g := newRetryRig(t, nil)
		g.cfg["default"].EndpointsProvider = Environment
		t.Setenv("TORRENT_WEB_SEEDER_SERVICE_HOST", podA)
		t.Setenv("TORRENT_WEB_SEEDER_SERVICE_PORT", "8080")
		a := g.pods[podA]
		a.down.Store(true)
		g.serve(podA, seederHandler(a, false, nil))
		time.AfterFunc(delay/3, func() { a.down.Store(false) })
		before := unanswered(t)
		start := time.Now()
		resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), delay)
		if err != nil {
			t.Fatalf("client got %v, want podA's answer once it is back", err)
		}
		_ = resp.Body.Close()
		if d := time.Since(start); d < delay {
			t.Fatalf("podA answered after %v, want after the %v delay", d, delay)
		}
		if ha, _ := g.hits(); ha != 1 || resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("client got %d, podA %d requests; want 206 and 1", resp.StatusCode, ha)
		}
		wantUnanswered(t, before, "success")
	})
}

// A client that leaves during the delay ends the request at once: as its
// own leaving (context.Canceled), with no attempt made. One that leaves
// during the attempt is counted as gone too, not as a failed retry.
func TestRetryUnansweredStopsWhenTheClientLeaves(t *testing.T) {
	t.Run("during the delay", func(t *testing.T) {
		g := newRetryRig(t, []string{podA})
		g.pods[podA].down.Store(true)
		g.serve(podA, seederHandler(g.pods[podA], false, nil))
		dials := g.countDials()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		before := unanswered(t)
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
		if n := dials.Load(); n != 1 {
			t.Fatalf("%d dials, want 1: no attempt after the client left", n)
		}
		wantUnanswered(t, before, "client_gone")
	})
	t.Run("during the attempt", func(t *testing.T) {
		g := newRetryRig(t, []string{podA, podB})
		g.pods[podA].down.Store(true)
		g.serve(podA, seederHandler(g.pods[podA], false, nil))
		ctx, cancel := context.WithCancel(context.Background())
		g.serve(podB, func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		})
		before := unanswered(t)
		resp, err := g.roundTrip(podARequest(t, ctx, http.MethodGet, nil), 0)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client got %v, want context.Canceled", err)
		}
		wantUnanswered(t, before, "client_gone")
	})
}

// A pod that could not be dialed goes on the shared ignore list, as in
// reconnectFn: podA refuses, the pod the retry reaches answers 503 busy,
// and the busy retry goes to the third pod, not back to podA, whose
// refusal would have left the client with the 503.
func TestRetryUnansweredThenBusyNotBackToTheRefusingPod(t *testing.T) {
	const podD = "10.233.62.4"
	g := newRetryRig(t, []string{podA, podB, podD})
	if rendezvousPick(retryHash, []string{podB, podD}) != podB || rendezvousPick(retryHash, []string{podA, podD}) != podA {
		t.Fatal("retryHash's order is not podA, podB, podD: the test proves nothing")
	}
	d := &retryPod{srv: httptest.NewUnstartedServer(nil)}
	t.Cleanup(d.srv.Close)
	g.pods[podD] = d
	a := g.pods[podA]
	a.down.Store(true)
	g.serve(podA, seederHandler(a, false, nil))
	g.serve(podB, seederRefuses(g.pods[podB], "busy"))
	g.serve(podD, seederHandler(d, false, nil))
	dials := g.countDials()
	before := unanswered(t)
	resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), 0)
	if err != nil {
		t.Fatalf("client got %v, want podD's answer", err)
	}
	wantUnanswered(t, before, "success")
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || err != nil || !bytes.Equal(got, retryPayload()[100:]) {
		t.Fatalf("client got %d %q, %d bytes (err %v); want podD's 206", resp.StatusCode, resp.Header.Get("X-Seeder-Reason"), len(got), err)
	}
	if n, hd := dials.Load(), d.hits.Load(); n != 3 || hd != 1 {
		t.Fatalf("%d dials, podD %d requests; want 3 (podA, podB, podD) and 1", n, hd)
	}
	if !g.sl.ignore.IsIgnored(podA) || g.sl.ignore.IsIgnored(podB) {
		t.Fatalf("ignored: podA %v, podB %v; want podA only", g.sl.ignore.IsIgnored(podA), g.sl.ignore.IsIgnored(podB))
	}
}

// A pod that reset the connection is not on the ignore list; the busy
// runner-up's own retry must still leave it out.
func TestRetryUnansweredResetThenBusyNotBackToTheResettingPod(t *testing.T) {
	const podD = "10.233.62.4"
	g := newRetryRig(t, []string{podA, podB, podD})
	if rendezvousPick(retryHash, []string{podB, podD}) != podB || rendezvousPick(retryHash, []string{podA, podD}) != podA {
		t.Fatal("retryHash's order is not podA, podB, podD: the test proves nothing")
	}
	d := &retryPod{srv: httptest.NewUnstartedServer(nil)}
	t.Cleanup(d.srv.Close)
	g.pods[podD] = d
	a := g.pods[podA]
	g.serve(podA, dropBeforeHeaders(a, true, ""))
	g.serve(podB, seederRefuses(g.pods[podB], "busy"))
	g.serve(podD, seederHandler(d, false, nil))
	before := unanswered(t)
	resp, err := g.roundTrip(podARequest(t, context.Background(), http.MethodGet, nil), 0)
	if err != nil {
		t.Fatalf("client got %v, want podD's answer", err)
	}
	wantUnanswered(t, before, "success")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("client got %d %q; want podD's 206", resp.StatusCode, resp.Header.Get("X-Seeder-Reason"))
	}
	if ha, hd := a.hits.Load(), d.hits.Load(); ha != 1 || hd != 1 {
		t.Fatalf("podA %d requests, podD %d; want 1 and 1", ha, hd)
	}
	if g.sl.ignore.IsIgnored(podA) {
		t.Fatal("podA ignored after a reset; only a refusal goes on the ignore list")
	}
}
