package services

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"

	"github.com/webtor-io/torrent-http-proxy/services/k8s"
)

// A panic in a lookup's loader is that request's error. Until lazymap
// 7a6c260 it escaped Get and left the key running for good: every later
// request for it blocked, and after 10 such panics nothing loaded at all.

// getWithin runs get with a deadline. A panic out of it comes back as an
// error, so the caller sees it is not a *lazymap.PanicError.
func getWithin(t *testing.T, get func() (*Location, error)) (*Location, error) {
	t.Helper()
	type res struct {
		l   *Location
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{err: fmt.Errorf("Get panicked: %v", r)}
			}
		}()
		l, err := get()
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		return r.l, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("Get hung")
		return nil, nil
	}
}

// panicOnRole makes NodeHash's lookup panic: a role sends the node step to
// the pool's NodesStat, and a pool built without one, as only a test builds
// it, dereferences nil there. The pods carry NodeNames, so the node step runs.
const panicOnRole = "panic"

func nodePodListing() (*corev1.Endpoints, error) {
	return &corev1.Endpoints{Subsets: []corev1.EndpointSubset{{Addresses: fakePods("worker1", 2, 0)}}}, nil
}

func TestPanickingLocationLoaderIsAnError(t *testing.T) {
	sl, ep, _, src, _, _ := repickRig(t)
	const svc = "nodehash"
	if _, err := ep.LazyMap.Get(svc, nodePodListing); err != nil {
		t.Fatal(err)
	}
	cfg := &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: NodeHash}
	claims := jwt.MapClaims{"role": panicOnRole}
	for i := 1; i <= 2; i++ {
		_, err := getWithin(t, func() (*Location, error) { return sl.Get(cfg, src, claims) })
		var pe *lazymap.PanicError
		if !errors.As(err, &pe) {
			t.Errorf("request %d: %v, want a *lazymap.PanicError", i, err)
		}
	}
	if st, _ := sl.LazyMap.Status(svc + src.InfoHash + panicOnRole); st != lazymap.None {
		t.Errorf("location cache holds the failed lookup: %v", st)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The probe cache stores errors (30 s), so a panicking probe is a failed
// one: its pod goes on the ignore list, the pick moves to the next pod, and
// the probe is not run again until the error expires.
func TestPanickingProbeIsAFailedProbe(t *testing.T) {
	const svc = "probe-panic"
	a, b := "10.0.0.1", "10.0.0.2"
	if rendezvousPick(repickHash, []string{a, b}) != a {
		a, b = b, a
	}
	ep := &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := ep.LazyMap.Get(svc, repickListing(a, b)); err != nil {
		t.Fatal(err)
	}
	var probesOfA atomic.Int32
	cl := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == a {
			probesOfA.Add(1)
			panic("probe exploded")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})}
	c := cli.NewContext(cli.NewApp(), flag.NewFlagSet("thp-test", flag.ContinueOnError), nil)
	sl := NewServiceLocationPool(c, cl, nil, ep)
	cfg := &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: Hash}

	l, err := getWithin(t, func() (*Location, error) { return sl.Get(cfg, &Source{InfoHash: repickHash}, nil) })
	if err != nil || l.IP.String() != b {
		t.Fatalf("pick: %v %v, want %s past the panicking probe of %s", l, err, b, a)
	}
	if !sl.ignore.IsIgnored(a) {
		t.Errorf("%s is not on the ignore list", a)
	}
	var pe *lazymap.PanicError
	if _, err := sl.probeChecker.Get(&Location{IP: net.ParseIP(a)}); !errors.As(err, &pe) {
		t.Errorf("probe of %s again: %v, want the stored *lazymap.PanicError", a, err)
	}
	if n := probesOfA.Load(); n != 1 {
		t.Errorf("%s probed %d times, want 1 (the error is stored)", a, n)
	}
}

// A panicking lookup answers 500 with an empty body, so PanicError's text
// stays in the log, and the log line carries the panic's stack: while the
// panic escaped, net/http logged it, and the one-line error names no site.
func TestPanickingLookupLogsItsStack(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 0))
	cfg := (*h.web.r.cfg)["default"]
	cfg.EndpointsProvider, cfg.Distribution = Kubernetes, NodeHash
	sl := h.web.r.svcLoc
	sl.ep = &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := sl.ep.LazyMap.Get(throttleTestSvc, nodePodListing); err != nil {
		t.Fatal(err)
	}
	sl.ignore = &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: time.Minute})}

	tok := handToken(t, hdrHS256, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "role": panicOnRole}, h.secret)
	r := httptest.NewRequest(http.MethodGet, "/"+harnessHash+"/Sintel/Sintel.mkv?token="+tok+"&api-key=k", nil)
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	rec := httptest.NewRecorder()
	logger, hook := logtest.NewNullLogger()
	h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))

	if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Errorf("answer %d %q, want 500 with no body", rec.Code, rec.Body.String())
	}
	for _, e := range hook.AllEntries() {
		if e.Message != "failed to get proxy" {
			continue
		}
		if st, _ := e.Data["stack"].(string); !strings.Contains(st, "distributeByNodeHash") {
			t.Errorf("stack field %q, want the panic site", st)
		}
		return
	}
	t.Fatal("no \"failed to get proxy\" line")
}

// Endpoints not backed by a pod list addresses without a NodeName. Every path
// to a pick routes them instead of dereferencing the nil: the cached lookup,
// where lazymap turns a panic into a 500, and the two that call getKubernetes
// directly, the re-pick of a cached pod that has left and GetFallback, where
// the panic reached net/http and closed the connection. With no node to rank,
// NodeHash picks by IP, as Hash does.
func TestNodelessEndpointsRoute(t *testing.T) {
	sl, ep, _, src, a, b := repickRig(t)
	sl.nn = "worker1" // and PreferLocalNode below: preferLocal reads NodeName too
	const svc = "nodeless"
	list := func(ips ...string) {
		ep.LazyMap.Drop(svc)
		if _, err := ep.LazyMap.Get(svc, repickListing(ips...)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: NodeHash, PreferLocalNode: true}
	pick := func(path string, get func() (*Location, error), want string) {
		t.Helper()
		if l, err := getWithin(t, get); err != nil || l == nil || l.IP.String() != want {
			t.Errorf("%s: %v %v, want %s", path, l, err, want)
		}
	}
	list(a, b)
	// a owns repickHash by IP (repickRig).
	pick("lookup", func() (*Location, error) { return sl.Get(cfg, src, nil) }, a)
	pick("GetFallback", func() (*Location, error) { return sl.GetFallback(cfg, src, []string{a}, nil) }, b)
	list(b) // a leaves; the location cache still holds it
	pick("re-pick", func() (*Location, error) { return sl.Get(cfg, src, nil) }, b)
}

// Beside pods, nodeless addresses (nil or empty NodeName) take no part in the
// node step, so the node set stays the one rest-api ranks; a nodeless pick is
// not local, and preferLocal swaps it for the local pod.
func TestNodelessAddressesBesidePods(t *testing.T) {
	node := "worker2"
	as := []corev1.EndpointAddress{{IP: "10.0.0.1"}, {IP: "10.0.0.2", NodeName: &node}, {IP: "10.0.0.3", NodeName: new(string)}}
	s := &ServiceLocation{nn: node}
	for _, h := range hashes(50) {
		if p, err := s.distributeByNodeHash(&Source{InfoHash: h}, as, nil); err != nil || p == nil || p.IP != "10.0.0.2" {
			t.Fatalf("%s: %v %v, want the one pod on a node", h[:8], p, err)
		}
	}
	l, err := s.preferLocal(&ServiceConfig{PreferLocalNode: true}, &Source{InfoHash: repickHash}, &as[0], as)
	if err != nil || l == nil || l.IP != "10.0.0.2" {
		t.Fatalf("nodeless pick: %v %v, want the local pod", l, err)
	}
}
