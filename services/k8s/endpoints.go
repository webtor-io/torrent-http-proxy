package k8s

import (
	"context"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
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

type Endpoints struct {
	lazymap.LazyMap[*corev1.Endpoints]
	cl        *Client
	namespace string
}

func NewEndpoints(c *cli.Context, cl *Client) *Endpoints {
	return &Endpoints{
		cl:        cl,
		namespace: c.String(endpointsNamespaceFlag),
		LazyMap: lazymap.New[*corev1.Endpoints](&lazymap.Config{
			Concurrency: 1,
			Expire:      60 * time.Second,
		}),
	}
}

func (s *Endpoints) Get(ctx context.Context, name string) (*corev1.Endpoints, error) {
	return s.LazyMap.Get(name, func() (*corev1.Endpoints, error) {
		log.Infof("getting k8s endpoints for %s", name)
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
