package services

import (
	"fmt"
	"net"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	grpcHostFlag = "grpc-host"
	grpcPortFlag = "grpc-port"
)

type GRPCServer struct {
	host string
	port int
	ln   net.Listener
	p    *GRPCProxy
}

func NewGRPCServer(c *cli.Context, p *GRPCProxy) *GRPCServer {
	return &GRPCServer{host: c.String(grpcHostFlag), port: c.Int(grpcPortFlag), p: p}
}

func RegisterGRPCFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  grpcHostFlag,
			Usage: "grpc listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  grpcPortFlag,
			Usage: "grpc listening port",
			Value: 50051,
		},
	)
}

func (s *GRPCServer) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "failed to grpc listen to tcp connection")
	}
	s.ln = ln
	p := s.p.Get()
	logrus.Infof("serving GRPC at %v", addr)
	return p.Serve(ln)
}

func (s *GRPCServer) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
