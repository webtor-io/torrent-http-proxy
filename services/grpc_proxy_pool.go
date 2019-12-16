package services

import (
	"fmt"
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
}

func NewGRPCProxyPool(claims *Claims) *GRPCProxyPool {
	return &GRPCProxyPool{claims: claims, expire: time.Duration(GRPC_PROXY_TTL) * time.Second}
}

func (s *GRPCProxyPool) Get(locw *LocationWrapper, logger *logrus.Entry) *grpcweb.WrappedGrpcServer {
	loc := locw.Location()
	key := "default"
	if !loc.Unavailable {
		key = fmt.Sprintf("%s%v", loc.IP.String(), loc.GRPC)
	}

	v, _ := s.sm.LoadOrStore(key, NewGRPCPRoxy(locw, s.claims, logger))
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

	return v.(*GRPCProxy).Get()
}
