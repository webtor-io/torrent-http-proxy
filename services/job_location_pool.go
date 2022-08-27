package services

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"

	"github.com/urfave/cli"

	"github.com/sirupsen/logrus"
)

type JobLocationPool struct {
	cl    *K8SClient
	l     *Locker
	sm    sync.Map
	c     *cli.Context
	locks sync.Map
}

func NewJobLocationPool(c *cli.Context, cl *K8SClient, l *Locker) *JobLocationPool {
	return &JobLocationPool{cl: cl, l: l, c: c}
}

func MakeJobID(cfg *JobConfig, params *InitParams) string {
	name := cfg.Name + "-" + params.InfoHash
	if params.Path != "" {
		h := sha1.New()
		h.Write([]byte(params.InfoHash + params.Path))
		pathHash := hex.EncodeToString(h.Sum(nil))
		name = cfg.Name + "-" + pathHash
	}
	if params.Extra != "" {
		h := sha1.New()
		h.Write([]byte(params.InfoHash + params.Path + params.Extra))
		extraHash := hex.EncodeToString(h.Sum(nil))
		name = cfg.Name + "-" + extraHash
	}
	return name
}

func (s *JobLocationPool) Get(cfg *JobConfig, params *InitParams, logger *logrus.Entry, purge bool, invoke bool, cl *Client) (*Location, error) {
	key := MakeJobID(cfg, params)
	clientName := "default"
	if cl != nil {
		clientName = cl.Name
	}
	logger = logger.WithFields(logrus.Fields{
		"jobID":      key,
		"jobName":    cfg.Name,
		"clientName": clientName,
	})
	// return &Location{Unavailable: true}, nil
	if !params.RunIfNotExists || !invoke {
		l, ok := s.sm.Load(key)
		if !ok {
			al, loaded := s.locks.LoadOrStore(key, NewAccessLock())
			if !loaded {
				logger.Info("setting lock")
				go func() {
					jl := NewJobLocation(s.c, cfg, params, s.cl, logger, s.l, cl)
					l, err := jl.Wait()
					if err != nil || l == nil {
						logger.Info("failed to wait for job location")
					} else {
						s.sm.LoadOrStore(key, jl)
						go func() {
							<-l.Expire
							s.sm.Delete(key)
							logger.Info("job deleted from pool")
						}()
					}
					logger.Info("unlocking")
					al.(*AccessLock).Unlock()
					s.locks.Delete(key)
				}()
			}
			logger.Info("wait to unlock")
			<-al.(*AccessLock).Unlocked()
			logger.Info("unlocked")
			l, ok := s.sm.Load(key)
			if !ok {
				return &Location{Unavailable: true}, nil
			}
			return l.(*JobLocation).Get()
		}
		return l.(*JobLocation).Get()
	}

	v, loaded := s.sm.LoadOrStore(key, NewJobLocation(s.c, cfg, params, s.cl, logger, s.l, cl))
	l, err := v.(*JobLocation).Invoke(purge)

	if !loaded && err == nil && l != nil {
		go func() {
			<-l.Expire
			s.sm.Delete(key)
			logger.Info("job deleted from pool")
		}()
	}
	if err != nil || l == nil {
		s.sm.Delete(key)
	}
	return l, err
}
