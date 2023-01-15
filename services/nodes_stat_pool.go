package services

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	nodesStatsTTL = 30
)

type NodesStatPool struct {
	mux    sync.Mutex
	expire time.Duration
	c      *cli.Context
	kcl    *K8SClient
	pcl    *PromClient
	l      *logrus.Entry
	stats  *NodesStat
}

func NewNodesStatPool(c *cli.Context, pcl *PromClient, kcl *K8SClient) *NodesStatPool {
	return &NodesStatPool{c: c, kcl: kcl, pcl: pcl, expire: time.Duration(nodesStatsTTL) * time.Second}
}

func (s *NodesStatPool) Get() ([]NodeStat, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.stats == nil {
		s.stats = NewNodesStat(s.c, s.pcl, s.kcl)
		go func() {
			<-time.After(s.expire)
			s.stats = nil
		}()
	}
	return s.stats.Get()
}
