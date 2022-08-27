package services

import (
	"sync"
	"time"

	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
)

const (
	ENDPOINTS_TTL = 30
)

type EndpointsPool struct {
	cl     *K8SClient
	c      *cli.Context
	sm     sync.Map
	expire time.Duration
}

func NewEndpointsPool(c *cli.Context, cl *K8SClient) *EndpointsPool {
	return &EndpointsPool{
		c:      c,
		cl:     cl,
		expire: time.Duration(ENDPOINTS_TTL) * time.Second,
	}
}

func (s *EndpointsPool) Get(name string) (*corev1.Endpoints, error) {
	v, loaded := s.sm.LoadOrStore(name, NewEndpoints(s.c, s.cl, name))
	if !loaded {
		go func() {
			<-time.After(s.expire)
			s.sm.Delete(name)
		}()
	}
	return v.(*Endpoints).Get()
}
