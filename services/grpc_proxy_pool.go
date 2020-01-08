package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"

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

func (s *GRPCProxyPool) Get(locw *LocationWrapper, logger *logrus.Entry) (*grpcweb.WrappedGrpcServer, error) {
	loc, err := locw.GetLocation(logger)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get location")
	}
	key := "default"
	if !loc.Unavailable {
		key = fmt.Sprintf("%s%v", loc.IP.String(), loc.GRPC)
	}
	if loc.GRPC == 0 {
		return nil, nil
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

	return v.(*GRPCProxy).Get(), nil
}
