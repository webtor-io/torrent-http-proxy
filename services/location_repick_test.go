package services

import (
	"testing"
	"time"

	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"

	"github.com/webtor-io/torrent-http-proxy/services/k8s"
)

// A pick cached a moment ago whose pod has since left the endpoints is not
// handed out for the rest of the location cache's 15 s: the next request
// goes to a pod that is still listed.
func TestCachedPickOfALeavingPodIsReplaced(t *testing.T) {
	const svc, hash = "repick", "08ada5a7a6183aae1e09d831df6748d566095a10"
	a, b := "10.0.0.1", "10.0.0.2"
	if o := rendezvousPick(hash, []string{a, b}); o != a {
		a, b = b, a
	}
	listing := func(ips ...string) func() (*corev1.Endpoints, error) {
		sub := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}}}
		for _, ip := range ips {
			sub.Addresses = append(sub.Addresses, corev1.EndpointAddress{IP: ip})
		}
		return func() (*corev1.Endpoints, error) {
			return &corev1.Endpoints{Subsets: []corev1.EndpointSubset{sub}}, nil
		}
	}
	ep := &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := ep.LazyMap.Get(svc, listing(a, b)); err != nil {
		t.Fatal(err)
	}
	probes := &ProbeChecker{LazyMap: lazymap.New[bool](&lazymap.Config{Expire: time.Hour})}
	for _, ip := range []string{a, b} {
		_, _ = probes.LazyMap.Get(ip, func() (bool, error) { return true, nil })
	}
	sl := &ServiceLocation{
		ep:           ep,
		LazyMap:      lazymap.New[*Location](&lazymap.Config{Expire: time.Minute}),
		ignore:       &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: 30 * time.Second})},
		probeChecker: probes,
	}
	cfg := &ServiceConfig{Name: svc, EndpointsProvider: Kubernetes, Distribution: Hash}
	src := &Source{InfoHash: hash}
	l, err := sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != a {
		t.Fatalf("first pick: %v %v, want the owner %s", l, err, a)
	}
	// a leaves the endpoints; the location cache still holds it.
	ep.LazyMap.Drop(svc)
	if _, err := ep.LazyMap.Get(svc, listing(b)); err != nil {
		t.Fatal(err)
	}
	l, err = sl.Get(cfg, src, nil)
	if err != nil || l.IP.String() != b {
		t.Fatalf("pick after %s left: %v %v, want %s", a, l, err, b)
	}
}

// Endpoints with no subsets (no pod is ready) make the service unavailable
// rather than panic on Subsets[0].
func TestNoReadyPodIsUnavailable(t *testing.T) {
	ep := &k8s.Endpoints{LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	_, _ = ep.LazyMap.Get("empty", func() (*corev1.Endpoints, error) { return &corev1.Endpoints{}, nil })
	sl := &ServiceLocation{
		ep:     ep,
		ignore: &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{Expire: 30 * time.Second})},
	}
	l, err := sl.getKubernetes(&ServiceConfig{Name: "empty", EndpointsProvider: Kubernetes, Distribution: Hash}, &Source{InfoHash: "08ada5a7a6183aae1e09d831df6748d566095a10"}, nil, nil)
	if err != nil || !l.Unavailable {
		t.Fatalf("got %+v %v, want unavailable", l, err)
	}
}
