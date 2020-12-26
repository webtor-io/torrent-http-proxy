package services

import (
	"sync"
	"time"

	"github.com/urfave/cli"
)

const (
	SUBDOMAINS_TTL = 60
)

type SubdomainsPool struct {
	sm     sync.Map
	timers sync.Map
	expire time.Duration
	c      *cli.Context
	k8s    *K8SClient
}

func NewSubdomainsPool(c *cli.Context, k8s *K8SClient) *SubdomainsPool {
	return &SubdomainsPool{c: c, k8s: k8s, expire: time.Duration(SUBDOMAINS_TTL) * time.Second}
}

func (s *SubdomainsPool) Get(infoHash string) ([]string, error) {
	key := infoHash
	v, _ := s.sm.LoadOrStore(key, NewSubdomains(s.c, s.k8s, infoHash))
	t, tLoaded := s.timers.LoadOrStore(key, time.NewTimer(s.expire))
	timer := t.(*time.Timer)
	if !tLoaded {
		go func() {
			<-timer.C
			s.sm.Delete(key)
			s.timers.Delete(key)
		}()
	}

	return v.(*Subdomains).Get()
}
