package services

import (
	"fmt"
	"net"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	GRPC_HOST_FLAG = "grpc-host"
	GRPC_PORT_FLAG = "grpc-port"
)

type GRPCServer struct {
	host string
	port int
	ln   net.Listener
	p    *GRPCProxy
}

func NewGRPCServer(c *cli.Context, p *GRPCProxy) *GRPCServer {
	return &GRPCServer{host: c.String(GRPC_HOST_FLAG), port: c.Int(GRPC_PORT_FLAG), p: p}
}

func RegisterGRPCFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  GRPC_HOST_FLAG,
		Usage: "grpc listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  GRPC_PORT_FLAG,
		Usage: "grpc listening port",
		Value: 50051,
	})
}

func (s *GRPCServer) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to grpc listen to tcp connection")
	}
	s.ln = ln
	p := s.p.Get()
	logrus.Infof("Serving GRPC at %v", addr)
	return p.Serve(ln)
}

func (s *GRPCServer) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
