package services

import (
	"context"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"

	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
)

const (
	endpointsNamespaceFlag = "endpoints-namespace"
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

type K8SEndpoints struct {
	lazymap.LazyMap[*corev1.Endpoints]
	cl        *K8SClient
	namespace string
}

func NewEndpoints(c *cli.Context, cl *K8SClient) *K8SEndpoints {
	return &K8SEndpoints{
		cl:        cl,
		namespace: c.String(endpointsNamespaceFlag),
		LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{
			Concurrency: 1,
			Expire:      60 * time.Second,
		}),
	}
}

func (s *K8SEndpoints) Get(ctx context.Context, name string) (*corev1.Endpoints, error) {
	return s.LazyMap.Get(name, func() (*corev1.Endpoints, error) {
		ctx2, cancel := context.WithTimeout(ctx, time.Second*10)
		defer cancel()
		cl, err := s.cl.Get()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get K8S client")
		}
		endpoints, err := cl.CoreV1().Endpoints(s.namespace).Get(ctx2, name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get endpoints for %s", name)
		}
		return endpoints, nil
	})
}
