package services

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"

	"github.com/webtor-io/torrent-http-proxy/services/k8s"
)

// A pick cached a moment ago whose pod has since left the endpoints is not
// handed out for the rest of the location cache's 15 s: the next request
// goes to a pod that is still listed.
const repickSvc, repickHash = "repick", "08ada5a7a6183aae1e09d831df6748d566095a10"

func repickListing(ips ...string) func() (*corev1.Endpoints, error) {
	sub := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}}}
	for _, ip := range ips {
		sub.Addresses = append(sub.Addresses, corev1.EndpointAddress{IP: ip})
	}
	return func() (*corev1.Endpoints, error) {
		return &corev1.Endpoints{Subsets: []corev1.EndpointSubset{sub}}, nil
	}
}

// repickRig lists pods a (the hash's owner) and b, both probed healthy, and
// has a picked and cached.
func repickRig(t *testing.T) (sl *ServiceLocation, ep *k8s.Endpoints, cfg *ServiceConfig, src *Source, a, b string) {
	const svc, hash = repickSvc, repickHash
	a, b = "10.0.0.1", "10.0.0.2"
	if o := rendezvousPick(hash, []string{a, b}); o != a {
		a, b = b, a
	}
	ep = &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := ep.LazyMap.Get(svc, repickListing(a, b)); err != nil {
		t.Fatal(err)
	}
	probes := &ProbeChecker{LazyMap: lazymap.New[bool](&lazymap.Config{Expire: time.Hour})}
	for _, ip := range []string{a, b} {
		_, _ = probes.LazyMap.Get(ip, func() (bool, error) { return true, nil })
	}
	sl = &ServiceLocation{
		ep:           ep,
		LazyMap:      lazymap.New[*Location](&lazymap.Config{Expire: time.Minute}),
		ignore:       &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: 30 * time.Second})},
		probeChecker: probes,
	}
	cfg = &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: Hash}
	src = &Source{InfoHash: hash}
	l, err := sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != a {
		t.Fatalf("first pick: %v %v, want the owner %s", l, err, a)
	}
	return
}

func TestCachedPickOfALeavingPodIsReplaced(t *testing.T) {
	sl, ep, cfg, src, a, b := repickRig(t)
	// a leaves the endpoints; the location cache still holds it.
	ep.LazyMap.Drop(repickSvc)
	if _, err := ep.LazyMap.Get(repickSvc, repickListing(b)); err != nil {
		t.Fatal(err)
	}
	l, err := sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != b {
		t.Fatalf("pick after %s left: %v %v, want %s", a, l, err, b)
	}
}

func TestCachedPickOfAnIgnoredPodIsReplaced(t *testing.T) {
	sl, _, cfg, src, a, b := repickRig(t)
	sl.Ignore(a)
	l, err := sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != b {
		t.Fatalf("pick after %s was ignored: %v %v, want %s", a, l, err, b)
	}
}

// Concurrent requests for one key right after its pod left all get the
// remaining pod. Replacing the cached pick with Drop and Get evicted the Gets
// racing it: a 500 "Evicted" for some of them.
func TestConcurrentRepickNeverFails(t *testing.T) {
	sl, ep, cfg, src, a, b := repickRig(t)
	relist := func(ips ...string) {
		ep.LazyMap.Drop(repickSvc)
		if _, err := ep.LazyMap.Get(repickSvc, repickListing(ips...)); err != nil {
			t.Fatal(err)
		}
	}
	const rounds, goroutines = 500, 32
	var failed atomic.Int64
	for r := 0; r < rounds; r++ {
		// a is listed and picked, then leaves: every goroutine finds the stale pick.
		relist(a, b)
		sl.LazyMap.Drop(repickSvc + repickHash)
		if l, err := sl.Get(cfg, src, nil); err != nil || l.IP.String() != a {
			t.Fatalf("round %d: pick %v %v, want %s", r, l, err, a)
		}
		relist(b)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if l, err := sl.Get(cfg, src, nil); err != nil || l.IP.String() != b {
					failed.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
	if n := failed.Load(); n > 0 {
		t.Fatalf("%d of %d requests right after %s left got an error or a pod other than %s", n, rounds*goroutines, a, b)
	}
}

// Endpoints that cannot be read (an API error on the fallback cache) say
// nothing about the cached pick: it is kept, not turned into an error.
func TestCachedPickSurvivesUnreadableEndpoints(t *testing.T) {
	sl, ep, cfg, src, a, _ := repickRig(t)
	ep.LazyMap = lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour, StoreErrors: true})
	_, _ = ep.LazyMap.Get(repickSvc, func() (*corev1.Endpoints, error) { return nil, errors.New("api unreachable") })
	l, err := sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != a {
		t.Fatalf("with endpoints unreadable: %v %v, want the cached %s", l, err, a)
	}
}
