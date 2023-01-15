package services

import (
	"net"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
)

type Ports struct {
	GRPC  int
	HTTP  int
	Probe int
}

type Location struct {
	Ports
	IP          net.IP
	Unavailable bool
	Expire      chan bool
}

type Resolver struct {
	cfg        *ConnectionsConfig
	svcLocPool *ServiceLocationPool
	jobLocPool *JobLocationPool
	baseURL    string
}

type InitParams struct {
	InfoHash       string
	OriginPath     string
	SourceURL      string
	Path           string
	Extra          string
	RunIfNotExists bool
}

type Init struct {
	InitParams       *InitParams
	ConnectionConfig *ConnectionConfig
}

func NewResolver(baseURL string, cfg *ConnectionsConfig, svcLocPool *ServiceLocationPool, jobLocPool *JobLocationPool) *Resolver {
	return &Resolver{
		cfg:        cfg,
		svcLocPool: svcLocPool,
		jobLocPool: jobLocPool,
		baseURL:    baseURL,
	}
}

func (s *Resolver) getInit(src *Source) *Init {
	var init *Init
	if src.Mod != nil {
		init = &Init{
			InitParams: &InitParams{
				InfoHash:       src.InfoHash,
				OriginPath:     src.OriginPath,
				Path:           src.Path,
				Extra:          src.Mod.Extra,
				SourceURL:      s.baseURL + "/" + src.InfoHash + src.Path + "?" + src.Query,
				RunIfNotExists: !s.cfg.GetMod(src.Mod.Type).CheckIgnorePaths(src.Mod.Path),
			},
			ConnectionConfig: s.cfg.GetMod(src.Mod.Type),
		}
	} else {
		init = &Init{
			InitParams: &InitParams{
				InfoHash:       src.InfoHash,
				RunIfNotExists: !s.cfg.GetMod(src.Type).CheckIgnorePaths(src.Path),
			},
			ConnectionConfig: s.cfg.GetMod(src.Type),
		}
		// logrus.WithField("init", init).WithField("src", src).Info("Got job init params")
	}
	return init
}

func (s *Resolver) process(i *Init, logger *logrus.Entry, purge bool, invoke bool, cl *Client) (*Location, error) {
	if i.ConnectionConfig.ConnectionType == ConnectionTypeService {
		return s.svcLocPool.Get(&i.ConnectionConfig.ServiceConfig, i.InitParams, purge)
	} else {
		return s.jobLocPool.Get(&i.ConnectionConfig.JobConfig, i.InitParams, logger, purge, invoke, cl)
	}
}

func (s *Resolver) Resolve(src *Source, logger *logrus.Entry, purge bool, invoke bool, cl *Client) (*Location, error) {
	start := time.Now()
	logger = logger.WithField("purge", purge)
	init := s.getInit(src)
	l, err := s.process(init, logger, purge, invoke, cl)
	logger = logger.WithField("duration", time.Since(start).Milliseconds())
	if err != nil {
		logger.WithError(err).Error("failed to resolve location")
		return nil, errors.Wrap(err, "failed to resolve location")
	}
	logger.WithField("location", l.IP).Info("location resolved")
	return l, nil
}
