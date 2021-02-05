package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	PROM_HOST = "prom-host"
	PROM_PORT = "prom-port"
)

type Prom struct {
	host string
	port int
	ln   net.Listener
}

func NewProm(c *cli.Context) *Prom {
	return &Prom{host: c.String(PROM_HOST), port: c.Int(PROM_PORT)}
}

func RegisterPromFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  PROM_HOST,
		Usage: "prometheus metrics listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  PROM_PORT,
		Usage: "prometheus metrics listening port",
		Value: 8082,
	})
}

func (s *Prom) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to web listen to tcp connection")
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	logrus.Infof("Serving Prom Metrics at %v", addr)
	return http.Serve(ln, mux)
}

func (s *Prom) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
