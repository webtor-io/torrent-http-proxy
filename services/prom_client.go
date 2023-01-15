package services

import (
	"sync"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/urfave/cli"
)

const (
	promAddrFlag = "prom-addr"
)

func RegisterPromClientFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   promAddrFlag,
			Usage:  "Prometheus connection address",
			Value:  "",
			EnvVar: "PROM_ADDR",
		},
	)
}

type PromClient struct {
	cl     v1.API
	addr   string
	inited bool
	err    error
	mux    sync.Mutex
}

func NewPromClient(c *cli.Context) *PromClient {
	return &PromClient{
		addr: c.String(promAddrFlag),
	}
}

func (s *PromClient) get() (v1.API, error) {
	if s.addr == "" {
		return nil, nil
	}
	cl, err := api.NewClient(api.Config{
		Address: s.addr,
	})
	if err != nil {
		return nil, err
	}
	return v1.NewAPI(cl), nil
}

func (s *PromClient) Get() (v1.API, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.cl, s.err
	}
	s.cl, s.err = s.get()
	s.inited = true
	return s.cl, s.err
}
