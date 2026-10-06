package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	"github.com/webtor-io/torrent-http-proxy/services/k8s"
	corev1 "k8s.io/api/core/v1"
)

// Two seeder pods on one node, as the endpoints list them; the stream
// starts on podA, the rendezvous owner of retryHash, so a fallback that
// did not leave podA out would pick it again.
const (
	podA      = "10.233.62.1"
	podB      = "10.233.62.2"
	retryHash = "3fae7e157abc0f0b448df82d80a67e361f5f4fc3"
	retrySvc  = "torrent-web-seeder"
	retrySize = 1000
	retryCut  = 100
)

type retryPod struct {
	srv  *httptest.Server
	hits atomic.Int32
	// down: dials to the pod are refused from now on.
	down atomic.Bool
}

type retryRig struct {
	sl   *ServiceLocation
	cfg  ServicesConfig
	tr   *http.Transport
	pods map[string]*retryPod
}

func retryPayload() []byte {
	b := make([]byte, retrySize)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// seederHandler answers like the seeder: 200 for the whole file, 206 from
// a Range start. It cuts the first response after retryCut bytes, and a
// resumed one too when cutResumes. Hits are counted before anything else.
func seederHandler(p *retryPod, cutResumes bool, onFirst func()) http.HandlerFunc {
	data := retryPayload()
	return func(w http.ResponseWriter, r *http.Request) {
		if p.hits.Add(1) == 1 && onFirst != nil {
			onFirst()
		}
		start, cut := 0, true
		if rg := r.Header.Get("Range"); rg != "" {
			s, _, _, _ := parseRange(rg)
			start, cut = int(s), cutResumes
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, retrySize-1, retrySize))
			w.Header().Set("Content-Length", strconv.Itoa(retrySize-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(retrySize))
			w.WriteHeader(http.StatusOK)
		}
		if !cut {
			_, _ = w.Write(data[start:])
			return
		}
		_, _ = w.Write(data[start:min(start+retryCut, retrySize)])
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// newRetryRig serves podA and podB from local servers, reached through a
// dialer that maps pod IP:8080 to them; listed are the pods in the
// endpoints' ready addresses.
func newRetryRig(t *testing.T, listed []string) *retryRig {
	t.Helper()
	g := &retryRig{pods: map[string]*retryPod{podA: {}, podB: {}}}
	for _, p := range g.pods {
		p.srv = httptest.NewUnstartedServer(nil) // started by serve
		t.Cleanup(p.srv.Close)
	}
	// A port nothing listens on: a real "connection refused".
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := l.Addr().String()
	_ = l.Close()
	g.tr = &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			host, _, _ := net.SplitHostPort(addr)
			p := g.pods[host]
			if p == nil || p.down.Load() {
				return d.DialContext(ctx, network, refused)
			}
			return d.DialContext(ctx, network, p.srv.Listener.Addr().String())
		},
	}
	node := "worker62"
	sub := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}}}
	for _, ip := range listed {
		sub.Addresses = append(sub.Addresses, corev1.EndpointAddress{IP: ip, NodeName: &node})
	}
	ep := &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := ep.LazyMap.Get(retrySvc, func() (*corev1.Endpoints, error) {
		return &corev1.Endpoints{Subsets: []corev1.EndpointSubset{sub}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	g.sl = &ServiceLocation{
		ep:      ep,
		LazyMap: lazymap.New[*Location](&lazymap.Config{Expire: time.Minute}),
		ignore:  &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: 30 * time.Second})},
	}
	g.cfg = ServicesConfig{"default": {Name: retrySvc, EndpointsProvider: Kubernetes, Distribution: Hash}}
	if o := rendezvousPick(retryHash, []string{podA, podB}); o != podA {
		t.Fatalf("retryHash is owned by %s, not podA: the fallback tests prove nothing", o)
	}
	return g
}

func (g *retryRig) serve(ip string, h http.HandlerFunc) {
	g.pods[ip].srv.Config.Handler = h
	g.pods[ip].srv.Start()
}

// get streams the file from podA through retryTransport.
func (g *retryRig) get(t *testing.T, maxRetries int) ([]byte, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+podA+":8080/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = WithRetryContext(req, &RetryContext{
		Src:               &Source{Type: "default", InfoHash: retryHash},
		SvcLoc:            g.sl,
		Cfg:               &g.cfg,
		Transport:         g.tr,
		ExternalTransport: g.tr,
		MaxRetries:        maxRetries,
	})
	resp, err := (&retryTransport{&redirectFollowingTransport{g.tr, g.tr}}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

func (g *retryRig) hits() (int32, int32) { return g.pods[podA].hits.Load(), g.pods[podB].hits.Load() }

func wantWhole(t *testing.T, got []byte, err error) {
	t.Helper()
	if err != nil || !bytes.Equal(got, retryPayload()) {
		t.Fatalf("client got %d bytes (err %v), want the whole %d-byte file in order", len(got), err, retrySize)
	}
}

// The seeder cut the stream (its stall guard, its write deadline) and is
// still a ready pod: the retry goes back to it, and no other pod loads the
// torrent. Until 2026-10 it went to the runner-up.
func TestRetryGoesBackToTheSamePodOnEOF(t *testing.T) {
	g := newRetryRig(t, []string{podA, podB})
	g.serve(podA, seederHandler(g.pods[podA], false, nil))
	g.serve(podB, seederHandler(g.pods[podB], false, nil))
	got, err := g.get(t, 3)
	wantWhole(t, got, err)
	if a, b := g.hits(); a != 2 || b != 0 {
		t.Fatalf("requests: podA %d, podB %d; want 2 and 0 (resumed on the same pod)", a, b)
	}
	if g.sl.ignore.IsIgnored(podA) {
		t.Fatal("podA is on the shared ignore list after answering")
	}
}

// The same-pod attempt is refused (the pod died after cutting the stream):
// the pod cannot be reached, so it goes on the shared ignore list, and the
// stream resumes on the other pod, within the same budget.
func TestRetryFallsBackAndIgnoresAPodThatRefuses(t *testing.T) {
	g := newRetryRig(t, []string{podA, podB})
	a := g.pods[podA]
	g.serve(podA, seederHandler(a, false, func() { a.down.Store(true) }))
	g.serve(podB, seederHandler(g.pods[podB], false, nil))
	got, err := g.get(t, 3)
	wantWhole(t, got, err)
	if ha, hb := g.hits(); ha != 1 || hb != 1 {
		t.Fatalf("requests: podA %d, podB %d; want 1 and 1", ha, hb)
	}
	if !g.sl.ignore.IsIgnored(podA) {
		t.Fatal("podA refused connections and is not on the shared ignore list")
	}
}

// The pod answers the same-pod attempt, but not with the rest of the file:
// the stream moves to the other pod, and podA, alive, is left out of this
// request only. On the shared list it would be off for every torrent for
// 30 s. Only a pod that cannot be dialed goes there: not one that answers
// an error, drops the connection before answering, or redirects (vault's
// presigned URL) to a target that refuses.
func TestRetryKeepsALivePodOffTheSharedIgnoreList(t *testing.T) {
	for _, c := range []struct {
		name string
		// resume answers podA's same-pod attempt; nil: the seeder cuts it.
		resume http.HandlerFunc
	}{
		{"cuts again", nil},
		{"answers 503", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}},
		{"closes before answering", func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}},
		{"redirects to a target that refuses", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://10.233.99.9:9000/file", http.StatusFound)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			a := g.pods[podA]
			seeder := seederHandler(a, true, nil)
			g.serve(podA, func(w http.ResponseWriter, r *http.Request) {
				if c.resume == nil || r.Header.Get("Range") == "" {
					seeder(w, r)
					return
				}
				a.hits.Add(1)
				c.resume(w, r)
			})
			g.serve(podB, seederHandler(g.pods[podB], false, nil))
			got, err := g.get(t, 3)
			wantWhole(t, got, err)
			if ha, hb := g.hits(); ha != 2 || hb != 1 {
				t.Fatalf("requests: podA %d, podB %d; want 2 (first, same-pod retry) and 1", ha, hb)
			}
			if g.sl.ignore.IsIgnored(podA) || g.sl.ignore.IsIgnored(podB) {
				t.Fatal("a pod that answered is on the shared ignore list")
			}
		})
	}
}

// podA is no longer a ready endpoint (being terminated): the retry goes
// straight to another pod.
func TestRetrySkipsAPodThatLeftTheEndpoints(t *testing.T) {
	g := newRetryRig(t, []string{podB})
	g.serve(podA, seederHandler(g.pods[podA], false, nil))
	g.serve(podB, seederHandler(g.pods[podB], false, nil))
	got, err := g.get(t, 3)
	wantWhole(t, got, err)
	if a, b := g.hits(); a != 1 || b != 1 {
		t.Fatalf("requests: podA %d, podB %d; want 1 and 1 (podA left the endpoints)", a, b)
	}
}

// The same-pod attempt takes one of the budget's attempts: with a budget
// of one and podA refusing it, the stream ends and podB is never asked.
func TestRetrySamePodAttemptCountsAgainstTheBudget(t *testing.T) {
	g := newRetryRig(t, []string{podA, podB})
	a := g.pods[podA]
	g.serve(podA, seederHandler(a, false, func() { a.down.Store(true) }))
	g.serve(podB, seederHandler(g.pods[podB], false, nil))
	got, err := g.get(t, 1)
	if err == nil || len(got) != retryCut {
		t.Fatalf("client got %d bytes, err %v; want the first %d and the original error", len(got), err, retryCut)
	}
	if _, b := g.hits(); b != 0 {
		t.Fatalf("podB got %d requests over a budget of one attempt", b)
	}
}

// Reconnects per stream never exceed maxRetries, whatever each attempt
// returns; a failed attempt after the client left is the last one.
func TestRetryAttemptsStayWithinMaxRetries(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, c := range []struct {
		name    string
		samePod bool
		err     error
		want    int
	}{
		{"same pod fails each time", true, refused, 3},
		{"another pod fails", false, refused, 1},
		{"same pod, client gone", true, errors.Wrap(context.Canceled, "retry request failed"), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			r := &retryingReadCloser{
				body: &dirtyReader{data: bytes.NewReader(make([]byte, 10)), err: io.ErrUnexpectedEOF},
				reconnectFn: func(int64) (io.ReadCloser, bool, error) {
					calls++
					if calls > 10 {
						return nil, false, c.err // stop a runaway loop
					}
					return nil, c.samePod, c.err
				},
				expected:   100,
				maxRetries: 3,
				logger:     logrus.WithField("test", true),
			}
			if _, err := io.ReadAll(r); err != io.ErrUnexpectedEOF {
				t.Fatalf("got %v, want the original error", err)
			}
			if calls != c.want {
				t.Fatalf("%d reconnects, want %d", calls, c.want)
			}
		})
	}
}

func TestIsDialError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{errors.Wrap(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}, "retry request failed"), true},
		// net's own cancel error (errCanceled) is context.Canceled to errors.Is.
		{&net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}, false},
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, false},
		{io.ErrUnexpectedEOF, false},
	} {
		if got := isDialError(c.err); got != c.want {
			t.Errorf("isDialError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
