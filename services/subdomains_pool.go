package services

import (
	"fmt"
	"sync"
	"time"

	"github.com/urfave/cli"
)

const (
	SUBDOMAINS_TTL = 30
)

type SubdomainsPool struct {
	sm     sync.Map
	timers sync.Map
	expire time.Duration
	c      *cli.Context
	k8s    *K8SClient
	nsp    *NodesStatPool
}

func NewSubdomainsPool(c *cli.Context, k8s *K8SClient, nsp *NodesStatPool) *SubdomainsPool {
	return &SubdomainsPool{c: c, k8s: k8s, nsp: nsp, expire: time.Duration(SUBDOMAINS_TTL) * time.Second}
}

func (s *SubdomainsPool) Get(infoHash string, skipActiveJobSearch bool, useCPU bool, useBandwidth bool, pool string) ([]string, error) {
	key := fmt.Sprintf("%v-%v-%v-%v-%v", infoHash, skipActiveJobSearch, useCPU, useBandwidth, pool)
	v, _ := s.sm.LoadOrStore(key, NewSubdomains(s.c, s.k8s, s.nsp, infoHash, skipActiveJobSearch, useCPU, useBandwidth, pool))
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
