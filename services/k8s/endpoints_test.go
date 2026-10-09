package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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
	cl := fake.NewClientset(testEndpoints("10.0.0.1", "10.0.0.2"))
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
