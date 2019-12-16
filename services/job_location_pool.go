package services

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"

	"github.com/urfave/cli"

	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"
)

type JobLocationPool struct {
	cl *K8SClient
	l  *Locker
	sm sync.Map
	c  *cli.Context
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
			return &Location{Unavailable: true}, nil
		}
	}

	v, loaded := s.sm.LoadOrStore(key, NewJobLocation(s.c, cfg, params, s.cl, logger, s.l))
	if !loaded && !params.RunIfNotExists {
		return nil, errors.Errorf("Running new job not allowed")
	}
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
		}
	}
	return l, err
}
