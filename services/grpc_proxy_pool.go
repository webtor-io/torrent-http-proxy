package services

import (
	"sync"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/sirupsen/logrus"
)

const (
	GRPC_PROXY_TTL = 60
)

type GRPCProxyPool struct {
	sm     sync.Map
	timers sync.Map
	claims *Claims
	expire time.Duration
	r      *Resolver
}

func NewGRPCProxyPool(claims *Claims, r *Resolver) *GRPCProxyPool {
	return &GRPCProxyPool{claims: claims, expire: time.Duration(GRPC_PROXY_TTL) * time.Second, r: r}
}

func (s *GRPCProxyPool) Get(src *Source, logger *logrus.Entry) (*grpcweb.WrappedGrpcServer, error) {
	key := src.GetKey()
	v, _ := s.sm.LoadOrStore(key, NewGRPCProxy(s.claims, s.r, src, logger))
	t, tLoaded := s.timers.LoadOrStore(key, time.NewTimer(s.expire))
	timer := t.(*time.Timer)
	if !tLoaded {
		go func() {
			<-timer.C
			s.sm.Delete(key)
			s.timers.Delete(key)
		}()
	} else {
		timer.Reset(s.expire)
	}

	return v.(*GRPCProxy).Get(), nil
}
