package services

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	NODES_STATS_TTL = 30
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

func NewNodesStatPool(c *cli.Context, pcl *PromClient, kcl *K8SClient, l *logrus.Entry) *NodesStatPool {
	return &NodesStatPool{c: c, kcl: kcl, pcl: pcl, expire: time.Duration(NODES_STATS_TTL) * time.Second}
}

func (s *NodesStatPool) Get() ([]NodeStat, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.stats == nil {
		s.stats = NewNodesStat(s.c, s.pcl, s.kcl, s.l)
		go func() {
			<-time.After(s.expire)
			s.stats = nil
		}()
	}
	return s.stats.Get()
}
