package services

import (
	"fmt"
	"net"
	"time"

	"github.com/dgrijalva/jwt-go"

	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
)

type Ports struct {
	HTTP  int
	Probe int
}

type Location struct {
	Ports
	IP          net.IP
	Unavailable bool
}

type Resolver struct {
	cfg    *ServicesConfig
	svcLoc *ServiceLocation
}

func NewResolver(cfg *ServicesConfig, svcLoc *ServiceLocation) *Resolver {
	return &Resolver{
		cfg:    cfg,
		svcLoc: svcLoc,
	}
}

func (s *Resolver) Resolve(src *Source, claims jwt.MapClaims, logger *logrus.Entry) (*Location, error) {
	start := time.Now()
	role, ok := claims["role"].(string)
	var cfg *ServiceConfig
	edgeType := src.GetEdgeType()
	if ok {
		cfg = s.cfg.GetMod(fmt.Sprintf("%s-%s", edgeType, role))
	}
	if cfg == nil {
		cfg = s.cfg.GetMod(edgeType)
	}
	l, err := s.svcLoc.Get(cfg, src, claims)
	logger = logger.WithField("duration", time.Since(start).Milliseconds())
	if err != nil {
		logger.WithError(err).Error("failed to resolve location")
		return nil, errors.Wrap(err, "failed to resolve location")
	}
	logger.WithField("location", l.IP).Info("location resolved")
	return l, nil
}
