package services

import "sync"

type ServiceLocationPool struct {
	sm sync.Map
}

func NewServiceLocationPool() *ServiceLocationPool {
	return &ServiceLocationPool{}
}

func (s *ServiceLocationPool) Get(cfg *ServiceConfig, purge bool) (*Location, error) {
	v, _ := s.sm.LoadOrStore(cfg.EnvName, NewServiceLocation(cfg))
	return v.(*ServiceLocation).Get(purge)
}
