package services

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli"
)

const (
	SUBDOMAINS_TTL = 30
)

type SubdomainsPool struct {
	sm     sync.Map
	expire time.Duration
	c      *cli.Context
	k8s    *K8SClient
	nsp    *NodesStatPool
}

func NewSubdomainsPool(c *cli.Context, k8s *K8SClient, nsp *NodesStatPool) *SubdomainsPool {
	return &SubdomainsPool{c: c, k8s: k8s, nsp: nsp, expire: time.Duration(SUBDOMAINS_TTL) * time.Second}
}

func (s *SubdomainsPool) Get(infoHash string, skipActiveJobSearch bool, useCPU bool, useBandwidth bool, pools []string) ([]NodeStatWithScore, []string, error) {
	key := fmt.Sprintf("%v-%v-%v-%v-%v", infoHash, skipActiveJobSearch, useCPU, useBandwidth, strings.Join(pools, "-"))
	v, loaded := s.sm.LoadOrStore(key, NewSubdomains(s.c, s.k8s, s.nsp, infoHash, skipActiveJobSearch, useCPU, useBandwidth, pools))
	if !loaded {
		go func() {
			<-time.After(s.expire)
			s.sm.Delete(key)
		}()
	}
	return v.(*Subdomains).Get()
}
