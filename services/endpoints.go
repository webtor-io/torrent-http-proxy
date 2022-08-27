package services

import (
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ENDPOINTS_NAMESPACE = "endpoints-namespace"
)

func RegisterEndpointsFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   ENDPOINTS_NAMESPACE,
		Usage:  "Endpoints namespace",
		Value:  "webtor",
		EnvVar: "ENDPOINTS_NAMESPACE",
	})
}

type Endpoints struct {
	cl        *K8SClient
	inited    bool
	err       error
	mux       sync.Mutex
	name      string
	namespace string
	endpoints *corev1.Endpoints
}

func NewEndpoints(c *cli.Context, cl *K8SClient, name string) *Endpoints {
	return &Endpoints{
		cl:        cl,
		name:      name,
		namespace: c.String(ENDPOINTS_NAMESPACE),
	}
}

func (s *Endpoints) get() (*corev1.Endpoints, error) {
	cl, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get K8S client")
	}
	endpoints, err := cl.CoreV1().Endpoints(s.namespace).Get(s.name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get endpoints")
	}
	return endpoints, nil
}

func (s *Endpoints) Get() (*corev1.Endpoints, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.endpoints, s.err
	}
	s.endpoints, s.err = s.get()
	s.inited = true
	return s.endpoints, s.err
}
