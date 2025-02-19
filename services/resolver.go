package services

import (
	"context"
	"github.com/dgrijalva/jwt-go"
	"net"
	"time"

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

func (s *Resolver) Resolve(ctx context.Context, src *Source, claims jwt.MapClaims, logger *logrus.Entry) (*Location, error) {
	start := time.Now()

	l, err := s.svcLoc.Get(ctx, s.cfg.GetMod(src.GetEdgeType()), src, claims)
	logger = logger.WithField("duration", time.Since(start).Milliseconds())
	if err != nil {
		logger.WithError(err).Error("failed to resolve location")
		return nil, errors.Wrap(err, "failed to resolve location")
	}
	logger.WithField("location", l.IP).Info("location resolved")
	return l, nil
}
