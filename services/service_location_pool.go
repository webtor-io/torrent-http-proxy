package services

import (
	"sync"
	"time"

	"github.com/urfave/cli"
)

const (
	serviceLocationTTL = 60
)

type ServiceLocationPool struct {
	sm     sync.Map
	expire time.Duration
	ep     *EndpointsPool
	c      *cli.Context
}

func NewServiceLocationPool(c *cli.Context, ep *EndpointsPool) *ServiceLocationPool {
	return &ServiceLocationPool{
		c:      c,
		ep:     ep,
		expire: time.Duration(serviceLocationTTL) * time.Second,
	}
}

func (s *ServiceLocationPool) Get(cfg *ServiceConfig, params *InitParams, purge bool) (*Location, error) {
	key := cfg.Name + params.InfoHash
	v, loaded := s.sm.LoadOrStore(key, NewServiceLocation(s.c, cfg, params, s.ep))
	if !loaded {
		go func() {
			<-time.After(s.expire)
			s.sm.Delete(key)
		}()
	}
	return v.(*ServiceLocation).Get(purge)
}
