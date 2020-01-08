package services

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/pkg/errors"
)

type ServiceLocation struct {
	loc    *Location
	cfg    *ServiceConfig
	inited bool
	err    error
	mux    sync.Mutex
}

func NewServiceLocation(cfg *ServiceConfig) *ServiceLocation {
	return &ServiceLocation{cfg: cfg, inited: false}
}

func (s *ServiceLocation) getPort(name string) (int, error) {
	envName := fmt.Sprintf("%s_SERVICE_PORT_%s", s.cfg.EnvName, name)
	portStr := os.Getenv(envName)
	if portStr != "" {
		port, err := strconv.ParseInt(portStr, 0, 0)
		if err != nil {
			return 0, errors.Errorf("Failed to parse port from environment name=%v value=%v", envName, portStr)
		}
		return int(port), nil
	}
	return 0, nil
}

func (s *ServiceLocation) get() (*Location, error) {
	envName := fmt.Sprintf("%s_SERVICE_HOST", s.cfg.EnvName)
	host := os.Getenv(envName)
	if host == "" {
		return nil, errors.Errorf("Failed to find host environment variable for service name=%v value=%v", envName, host)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.Errorf("Failed to parse host=%v to ip", host)
	}
	http, err := s.getPort("HTTP")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get http port")
	}
	grpc, err := s.getPort("GRPC")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get grpc port")
	}
	probe, err := s.getPort("PROBE")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get probe port")
	}
	return &Location{
		IP: ip,
		Ports: Ports{
			HTTP:  http,
			GRPC:  grpc,
			Probe: probe,
		},
		Unavailable: false,
	}, nil
}

func (s *ServiceLocation) Get(purge bool) (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if purge {
		s.inited = false
	}
	if s.inited {
		return s.loc, s.err
	}
	s.loc, s.err = s.get()
	s.inited = true
	return s.loc, s.err
}
