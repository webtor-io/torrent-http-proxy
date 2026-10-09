package k8s

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
)

const (
	endpointsNamespaceFlag = "endpoints-namespace"

	// endpointsWatchWarnAfter is how long the watch may take to sync before
	// the fallback is reported as lasting.
	endpointsWatchWarnAfter = 30 * time.Second
)

func RegisterEndpointsFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   endpointsNamespaceFlag,
			Usage:  "K8SEndpoints namespace",
			Value:  "webtor",
			EnvVar: "ENDPOINTS_NAMESPACE",
		},
	)
}

// Endpoints serves the namespace's Endpoints from a watch, so a pod leaves
// routing as soon as it leaves its Endpoints (it starts terminating). Until
// 2026-10 every lookup was a GET cached for 60 s: a deleted pod kept getting
// new requests for up to 75 s with the location cache, 16-38 of them 502 on
// a transcoder rollout, and every service behind the proxy needed a preStop
// longer than that.
//
// Until the watch has synced, and for good should it never sync (RBAC
// without list/watch on endpoints), lookups fall back to that GET cache.
type Endpoints struct {
	*lazymap.LazyMap[*corev1.Endpoints]
	cl        *Client
	namespace string
	once      sync.Once
	lister    atomic.Pointer[listersv1.EndpointsNamespaceLister]
}

func NewEndpoints(c *cli.Context, cl *Client) *Endpoints {
	return &Endpoints{
		cl:        cl,
		namespace: c.String(endpointsNamespaceFlag),
		LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
	}
}

// Get returns the Endpoints named name. The object is shared with the watch
// cache: read it, never modify it.
func (s *Endpoints) Get(name string) (*corev1.Endpoints, error) {
	s.once.Do(s.watch)
	if l := s.lister.Load(); l != nil {
		endpoints, err := (*l).Get(name)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get k8s endpoints for %s", name)
		}
		return endpoints, nil
	}
	return s.LazyMap.Get(name, func() (*corev1.Endpoints, error) {
		log.Infof("getting k8s endpoints for %s", name)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		cl, err := s.cl.Get()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get k8s client")
		}
		endpoints, err := cl.CoreV1().Endpoints(s.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get k8s endpoints for %s", name)
		}
		return endpoints, nil
	})
}

// watch starts the namespace's Endpoints informer for the process lifetime
// and switches Get to it once it has synced.
func (s *Endpoints) watch() {
	if s.cl == nil {
		return
	}
	cl, err := s.cl.Get()
	if err != nil {
		log.WithError(err).Warn("no k8s client for the endpoints watch, routing stays on the 60 s endpoints cache")
		return
	}
	s.startWatch(cl)
}

func (s *Endpoints) startWatch(cl kubernetes.Interface) {
	f := informers.NewSharedInformerFactoryWithOptions(cl, 0, informers.WithNamespace(s.namespace))
	inf := f.Core().V1().Endpoints()
	l := inf.Lister().Endpoints(s.namespace)
	f.Start(nil)
	go func() {
		t := time.AfterFunc(endpointsWatchWarnAfter, func() {
			log.Warnf("endpoints watch in %s has not synced in %v (list/watch on endpoints allowed?), routing stays on the 60 s endpoints cache", s.namespace, endpointsWatchWarnAfter)
		})
		cache.WaitForCacheSync(nil, inf.Informer().HasSynced)
		t.Stop()
		s.lister.Store(&l)
		log.Infof("endpoints watch in %s synced, routing follows pod changes as they happen", s.namespace)
	}()
}
