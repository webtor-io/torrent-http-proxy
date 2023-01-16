package services

import (
	"github.com/urfave/cli"
	"sync"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/sirupsen/logrus"
)

const (
	grpcProxyTTL = 60
)

type HTTPGRPCProxyPool struct {
	sm      sync.Map
	timers  sync.Map
	claims  *Claims
	expire  time.Duration
	r       *Resolver
	baseURL string
	c       *cli.Context
}

func NewHTTPGRPCProxyPool(c *cli.Context, bu string, claims *Claims, r *Resolver) *HTTPGRPCProxyPool {
	return &HTTPGRPCProxyPool{
		baseURL: bu,
		claims:  claims,
		expire:  time.Duration(grpcProxyTTL) * time.Second,
		r:       r,
		c:       c,
	}
}

func (s *HTTPGRPCProxyPool) Get(src *Source, logger *logrus.Entry) (*grpcweb.WrappedGrpcServer, error) {
	key := src.GetKey()
	v, _ := s.sm.LoadOrStore(key, NewHTTPGRPCProxy(NewGRPCProxy(s.c, s.baseURL, s.claims, s.r, src, nil, logger)))
	t, tLoaded := s.timers.LoadOrStore(key, time.NewTimer(s.expire))
	timer := t.(*time.Timer)
	proxy := v.(*HTTPGRPCProxy)
	if !tLoaded {
		go func(timer *time.Timer, proxy *HTTPGRPCProxy) {
			<-timer.C
			s.sm.Delete(key)
			s.timers.Delete(key)
			proxy.Close()
		}(timer, proxy)
	} else {
		timer.Reset(s.expire)
	}

	return v.(*HTTPGRPCProxy).Get(), nil
}
