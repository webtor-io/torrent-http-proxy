package services

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"time"

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

func (s *JobLocationPool) Get(cfg *JobConfig, params *InitParams, logger *logrus.Entry, purge bool) (*Location, error) {
	key := MakeJobID(cfg, params)
	logger = logger.WithFields(logrus.Fields{
		"jobID":   key,
		"jobName": cfg.Name,
	})
	// return &Location{Unavailable: true}, nil
	if !params.RunIfNotExists {
		_, ok := s.sm.Load(key)
		if !ok {
			expire := 10 * time.Minute
			al, loaded := s.locks.LoadOrStore(key, NewAccessLock(expire))
			if loaded {
				al.(*AccessLock).Reset()
			}
			logger.Info("Setting lock")
			select {
			case <-time.After(expire):
				break
			case <-al.(*AccessLock).Unlocked():
				logger.Info("Unlocked")
				if !loaded {
					logger.Info("Lock deleted")
					s.locks.Delete(key)
				}
				break
			}
			l, ok := s.sm.Load(key)
			if !ok {
				return &Location{Unavailable: true}, nil
			}
			return l.(*JobLocation).Get(false)
		}
	}

	v, loaded := s.sm.LoadOrStore(key, NewJobLocation(s.c, cfg, params, s.cl, logger, s.l))
	l, err := v.(*JobLocation).Get(purge)

	if !loaded {
		if err != nil || l == nil {
			defer s.sm.Delete(key)
			logger.Info("Failed to get job location")
		} else {
			go func() {
				err := v.(*JobLocation).WaitFinish()
				if err != nil {
					logger.WithError(err).Error("Failed to wait for pod finish")
				}
				l.Active = false
				s.sm.Delete(key)
				logger.Info("Job deleted from pool")
			}()
			al, ok := s.locks.Load(key)
			if ok {
				al.(*AccessLock).Unlock()
			}
		}
	}
	return l, err
}
