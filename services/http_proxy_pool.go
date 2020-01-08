package services

import (
	"fmt"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	HTTP_PROXY_TTL = 60
)

type HTTPProxyPool struct {
	sm     sync.Map
	timers sync.Map
	expire time.Duration
}

func NewHTTPProxyPool() *HTTPProxyPool {
	return &HTTPProxyPool{expire: time.Duration(HTTP_PROXY_TTL) * time.Second}
}

func (s *HTTPProxyPool) Get(locw *LocationWrapper, logger *logrus.Entry) (*httputil.ReverseProxy, error) {
	loc, err := locw.GetLocation(logger)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get location")
	}
	if loc.HTTP == 0 {
		return nil, nil
	}

	key := "default"

	if !loc.Unavailable {
		key = fmt.Sprintf("%s%v", loc.IP.String(), loc.HTTP)
	}

	v, _ := s.sm.LoadOrStore(key, NewHTTPProxy(locw, logger))
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

	return v.(*HTTPProxy).Get()
}
