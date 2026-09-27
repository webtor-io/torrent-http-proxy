package services

import (
	"errors"
	"flag"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
)

// endpointsOf is an Endpoints object as the API returns it: ready pods in
// addresses, the rest in notReadyAddresses.
func endpointsOf(ready, notReady []string) *corev1.Endpoints {
	s := corev1.EndpointSubset{}
	for _, ip := range ready {
		s.Addresses = append(s.Addresses, corev1.EndpointAddress{IP: ip})
	}
	for _, ip := range notReady {
		s.NotReadyAddresses = append(s.NotReadyAddresses, corev1.EndpointAddress{IP: ip})
	}
	return &corev1.Endpoints{Subsets: []corev1.EndpointSubset{s}}
}

// loopbackCallers is a snapshot with the harness caller pod, and loopback
// too when internal: a test over a real connection comes from loopback.
func loopbackCallers(internal bool) *map[netip.Addr]string {
	m := map[netip.Addr]string{netip.MustParseAddr(harnessCallerIP): harnessCallerSvc}
	if internal {
		m[netip.MustParseAddr("127.0.0.1")] = harnessCallerSvc
		m[netip.MustParseAddr("::1")] = harnessCallerSvc
	}
	return &m
}

// fakeEndpoints serves a settable list per service, or an error.
type fakeEndpoints struct {
	mu    sync.Mutex
	eps   map[string]*corev1.Endpoints
	err   error
	calls atomic.Int64
}

func (f *fakeEndpoints) set(name string, ep *corev1.Endpoints, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eps == nil {
		f.eps = map[string]*corev1.Endpoints{}
	}
	f.eps[name] = ep
	f.err = err
}

func (f *fakeEndpoints) get(name string) (*corev1.Endpoints, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.eps[name], nil
}

// manualClock is a clock the test moves; refresh reads it under its lock.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func expectCaller(t *testing.T, ic *InternalCallers, remoteAddr, want string) {
	t.Helper()
	name, ok := ic.Caller(remoteAddr)
	if want == "" {
		if ok {
			t.Errorf("%q reads internal (%q), want external", remoteAddr, name)
		}
		return
	}
	if !ok || name != want {
		t.Errorf("%q reads %q, internal=%v; want internal %q", remoteAddr, name, ok, want)
	}
}

// Every pod of every routed service, ready or not, by the address the
// connection came from; anything else, and anything that is not a peer
// address, is external.
func TestInternalCallersByPeerAddress(t *testing.T) {
	f := &fakeEndpoints{}
	f.set("nginx-vod", endpointsOf([]string{"10.0.70.5"}, []string{"10.0.71.9"}), nil)
	f.set("content-transcoder", endpointsOf([]string{"10.0.72.3", "::ffff:10.0.73.1"}, nil), nil)
	ic := newInternalCallers([]string{"content-transcoder", "nginx-vod"}, f.get, nil, time.Hour, time.Hour, time.Now)

	// Before the first refresh nothing is known: external.
	expectCaller(t, ic, "10.0.70.5:41234", "")

	ic.refresh()
	cases := []struct{ addr, want string }{
		{"10.0.70.5:41234", "nginx-vod"},
		// A dual-stack listener hands IPv4 peers over as IPv4-mapped.
		{"[::ffff:10.0.70.5]:41234", "nginx-vod"},
		// Not ready is still our pod finishing what it started.
		{"10.0.71.9:1", "nginx-vod"},
		{"10.0.72.3:80", "content-transcoder"},
		// Listed and seen in either spelling of IPv4 alike.
		{"10.0.73.1:80", "content-transcoder"},
		// A zone is part of the address: no pod is link-local.
		{"[fe80::1%eth0]:80", ""},
		// A neighbour of a listed pod (ingress-nginx, say) is not one.
		{"10.0.70.6:41234", ""},
		{"192.0.2.1:1234", ""},
		// Not a RemoteAddr: fail closed.
		{"10.0.70.5", ""},
		{"", ""},
		{"nginx-vod:80", ""},
	}
	for _, c := range cases {
		expectCaller(t, ic, c.addr, c.want)
	}
	var none *InternalCallers
	if _, ok := none.Caller("10.0.70.5:41234"); ok {
		t.Error("a nil InternalCallers knows a caller, want none")
	}
}

// A pod replaced by one on a new address: the new one is a caller from the
// next refresh on, the old one for the linger (it may still be draining),
// then external.
func TestInternalCallersPodReplaced(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_800_000_000, 0)}
	f := &fakeEndpoints{}
	f.set("nginx-vod", endpointsOf([]string{"10.0.70.5", "10.0.70.7"}, nil), nil)
	ic := newInternalCallers([]string{"nginx-vod"}, f.get, nil, time.Hour, time.Minute, clk.now)
	ic.refresh()
	expectCaller(t, ic, "10.0.70.5:1", "nginx-vod")

	f.set("nginx-vod", endpointsOf([]string{"10.0.70.7", "10.0.70.9"}, nil), nil)
	clk.add(time.Second)
	ic.refresh()
	expectCaller(t, ic, "10.0.70.9:1", "nginx-vod")
	expectCaller(t, ic, "10.0.70.5:1", "nginx-vod")

	clk.add(time.Minute)
	ic.refresh()
	expectCaller(t, ic, "10.0.70.5:1", "")
	expectCaller(t, ic, "10.0.70.7:1", "nginx-vod")
	expectCaller(t, ic, "10.0.70.9:1", "nginx-vod")
	if got := len(*ic.addrs.Load()); got != 2 {
		t.Errorf("%d addresses published, want 2", got)
	}
	if got := gaugeValue(t, promInternalCallers); got != 2 {
		t.Errorf("webtor_http_proxy_internal_callers = %v, want 2", got)
	}
}

// Endpoints that fail keep what was listed for the linger and no longer: a
// caller then reads external, never an unknown address internal.
func TestInternalCallersEndpointsErrorFailsClosed(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_800_000_000, 0)}
	f := &fakeEndpoints{}
	f.set("nginx-vod", endpointsOf([]string{"10.0.70.5"}, nil), nil)
	ic := newInternalCallers([]string{"nginx-vod"}, f.get, nil, time.Hour, time.Minute, clk.now)
	ic.refresh()

	f.set("nginx-vod", nil, errors.New("apiserver unavailable"))
	clk.add(30 * time.Second)
	ic.refresh()
	expectCaller(t, ic, "10.0.70.5:1", "nginx-vod")

	clk.add(31 * time.Second)
	ic.refresh()
	expectCaller(t, ic, "10.0.70.5:1", "")
}

// A service whose endpoints fail is asked for again after the retry, not
// every refresh; once they answer, every refresh reads them.
func TestInternalCallersRetryAfterError(t *testing.T) {
	clk := &manualClock{t: time.Unix(1_800_000_000, 0)}
	f := &fakeEndpoints{}
	f.set("subtitle-translate", nil, errors.New(`endpoints "subtitle-translate" not found`))
	ic := newInternalCallers([]string{"subtitle-translate"}, f.get, nil, time.Hour, time.Minute, clk.now)
	ic.refresh()
	for i := 0; i < 10; i++ {
		clk.add(time.Second)
		ic.refresh()
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d reads within the retry, want 1", n)
	}
	f.set("subtitle-translate", endpointsOf([]string{"10.0.74.2"}, nil), nil)
	clk.add(internalCallersRetry)
	ic.refresh()
	expectCaller(t, ic, "10.0.74.2:1", "subtitle-translate")
	clk.add(time.Second)
	ic.refresh()
	if n := f.calls.Load(); n != 3 {
		t.Errorf("%d reads, want 3: one failed, then one per refresh", n)
	}
}

// The services are those routing locates through Kubernetes endpoints, each
// once; an Environment entry's address is a destination, not a caller.
func TestInternalCallerNames(t *testing.T) {
	cfg := &ServicesConfig{
		"default":  {Name: "torrent-web-seeder", EndpointsProvider: Kubernetes},
		"vod":      {Name: "nginx-vod", EndpointsProvider: Kubernetes},
		"vod-paid": {Name: "nginx-vod", EndpointsProvider: Kubernetes},
		"ext":      {Name: "external-proxy", EndpointsProvider: Environment},
	}
	want := []string{"nginx-vod", "torrent-web-seeder"}
	if got := internalCallerNames(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("names %v, want %v", got, want)
	}
}

// --internal-caller-addrs: exact addresses, internal from the start, no
// refresh needed. Anything that is not an address fails startup.
func TestInternalCallerAddrsFlag(t *testing.T) {
	newCtx := func(v string) *cli.Context {
		set := flag.NewFlagSet("thp-test", flag.ContinueOnError)
		for _, f := range RegisterInternalCallersFlags(nil) {
			f.Apply(set)
		}
		if err := set.Set(internalCallerAddrsFlag, v); err != nil {
			t.Fatal(err)
		}
		return cli.NewContext(cli.NewApp(), set, nil)
	}
	ic, err := NewInternalCallers(newCtx(" 127.0.0.1, ::1 ,::ffff:127.0.0.3"), &ServicesConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectCaller(t, ic, "127.0.0.1:5000", configuredCaller)
	expectCaller(t, ic, "[::1]:5000", configuredCaller)
	expectCaller(t, ic, "127.0.0.3:5000", configuredCaller)
	expectCaller(t, ic, "127.0.0.2:5000", "")
	ic.Start() // nothing to refresh: returns without a goroutine
	ic.Close()
	for _, bad := range []string{"10.0.0.0/8", "localhost", "10.0.70.5:80"} {
		if _, err := NewInternalCallers(newCtx(bad), &ServicesConfig{}, nil); err == nil || !strings.Contains(err.Error(), internalCallerAddrsFlag) {
			t.Errorf("%q: err %v, want a startup error naming the flag", bad, err)
		}
	}
}

// Start refreshes in the background until Close.
func TestInternalCallersStartRefreshesUntilClose(t *testing.T) {
	f := &fakeEndpoints{}
	f.set("nginx-vod", endpointsOf([]string{"10.0.70.5"}, nil), nil)
	ic := newInternalCallers([]string{"nginx-vod"}, f.get, nil, 5*time.Millisecond, time.Hour, time.Now)
	ic.Start()
	ic.Start() // once
	eventually(t, "the first refresh", func() bool { _, ok := ic.Caller("10.0.70.5:1"); return ok })
	f.set("nginx-vod", endpointsOf([]string{"10.0.70.5", "10.0.70.9"}, nil), nil)
	eventually(t, "a new pod", func() bool { _, ok := ic.Caller("10.0.70.9:1"); return ok })
	ic.Close()
	ic.Close()
	time.Sleep(20 * time.Millisecond) // a refresh in progress at Close ends
	n := f.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := f.calls.Load(); got != n {
		t.Errorf("%d endpoint reads after Close, want none", got-n)
	}
}
