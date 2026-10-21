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

func TestPanickingLocationLoaderIsAnError(t *testing.T) {
	sl, ep, _, src, a, b := repickRig(t)
	// repickListing's addresses have no NodeName, which NodeHash dereferences.
	const svc = "nodehash"
	if _, err := ep.LazyMap.Get(svc, repickListing(a, b)); err != nil {
		t.Fatal(err)
	}
	cfg := &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: NodeHash}
	for i := 1; i <= 2; i++ {
		_, err := getWithin(t, func() (*Location, error) { return sl.Get(cfg, src, nil) })
		var pe *lazymap.PanicError
		if !errors.As(err, &pe) {
			t.Errorf("request %d: %v, want a *lazymap.PanicError", i, err)
		}
	}
	if st, _ := sl.LazyMap.Status(svc + src.InfoHash); st != lazymap.None {
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
	// No NodeName, which NodeHash dereferences.
	if _, err := sl.ep.LazyMap.Get(throttleTestSvc, repickListing("10.0.0.1", "10.0.0.2")); err != nil {
		t.Fatal(err)
	}
	sl.ignore = &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: time.Minute})}

	tok := handToken(t, hdrHS256, map[string]any{"exp": time.Now().Add(time.Hour).Unix()}, h.secret)
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
