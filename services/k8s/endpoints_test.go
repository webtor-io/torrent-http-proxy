package k8s

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// watchedClient serves Endpoints from a fake clientset and closes the
// returned channel once the informer's watch is registered: an update made
// before that would be lost, the fake does not replay it.
func watchedClient(objs ...runtime.Object) (*fake.Clientset, <-chan struct{}) {
	cl := fake.NewClientset(objs...)
	started := make(chan struct{})
	var once sync.Once
	cl.PrependWatchReactor("*", func(a clienttesting.Action) (bool, watch.Interface, error) {
		w, err := cl.Tracker().Watch(a.GetResource(), a.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(started) })
		return true, w, nil
	})
	return cl, started
}

func testEndpoints(ips ...string) *corev1.Endpoints {
	sub := corev1.EndpointSubset{Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}}}
	for _, ip := range ips {
		sub.Addresses = append(sub.Addresses, corev1.EndpointAddress{IP: ip})
	}
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "webtor"},
		Subsets:    []corev1.EndpointSubset{sub},
	}
}

// A pod that leaves its Endpoints (it is terminating) leaves Get at once,
// not when a cached listing expires: the old 60 s cache, primed here with
// both pods, would keep returning it.
func TestWatchDropsALeavingPodAtOnce(t *testing.T) {
	cl, started := watchedClient(testEndpoints("10.0.0.1", "10.0.0.2"))
	s := &Endpoints{namespace: "webtor", LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := s.LazyMap.Get("svc", func() (*corev1.Endpoints, error) {
		return testEndpoints("10.0.0.1", "10.0.0.2"), nil
	}); err != nil {
		t.Fatal(err)
	}
	s.once.Do(func() { s.startWatch(cl) })
	for deadline := time.Now().Add(5 * time.Second); s.lister.Load() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the watch did not sync in 5 s")
		}
	}
	<-started
	if _, err := cl.CoreV1().Endpoints("webtor").Update(context.Background(), testEndpoints("10.0.0.1"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	var n int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		ep, err := s.Get("svc")
		if err != nil {
			t.Fatal(err)
		}
		if n = len(ep.Subsets[0].Addresses); n == 1 {
			return
		}
	}
	t.Fatalf("Get still lists %d pods 2 s after one left the Endpoints", n)
}

// Until the watch's first list completes Get serves the fallback cache, not
// the empty informer store (NotFound for every service: a 500 on every
// request of a starting proxy).
func TestGetServesTheFallbackUntilTheWatchSyncs(t *testing.T) {
	cl, _ := watchedClient(testEndpoints("10.0.0.1"))
	release := make(chan struct{})
	cl.PrependReactor("list", "endpoints", func(clienttesting.Action) (bool, runtime.Object, error) {
		<-release
		return false, nil, nil
	})
	s := &Endpoints{namespace: "webtor", LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{Expire: time.Hour})}
	if _, err := s.LazyMap.Get("svc", func() (*corev1.Endpoints, error) {
		return testEndpoints("10.0.0.1", "10.0.0.2"), nil
	}); err != nil {
		t.Fatal(err)
	}
	s.once.Do(func() { s.startWatch(cl) })
	time.Sleep(200 * time.Millisecond)
	ep, err := s.Get("svc")
	if err != nil || len(ep.Subsets[0].Addresses) != 2 {
		t.Fatalf("before the first list: %v, %v; want the fallback's 2 pods", ep, err)
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ep, err := s.Get("svc"); err == nil && len(ep.Subsets[0].Addresses) == 1 {
			return
		}
	}
	t.Fatal("Get did not switch to the watch within 5 s of the list completing")
}
