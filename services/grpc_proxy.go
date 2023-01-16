package services

import (
	"context"
	"fmt"
	"github.com/urfave/cli"
	"net/url"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	grpcMiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
)

const (
	grpcProxyRedialTriesFlag  = "grpc-proxy-redial-tries"
	grpcProxyRedialDelayFlag  = "grpc-proxy-redial-delay"
	grpcProxyUnaryTimeoutFlag = "grpc-proxy-unary-timeout"
)

func RegisterGRPCProxyFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   grpcProxyRedialTriesFlag,
			Usage:  "GRPC proxy redial tries",
			Value:  2,
			EnvVar: "GRPC_PROXY_REDIAL_TRIES",
		},
		cli.IntFlag{
			Name:   grpcProxyRedialDelayFlag,
			Usage:  "GRPC proxy redial delay (sec)",
			Value:  1,
			EnvVar: "GRPC_PROXY_REDIAL_DELAY",
		},
		cli.IntFlag{
			Name:   grpcProxyUnaryTimeoutFlag,
			Usage:  "GRPC proxy unary timeout (sec)",
			Value:  30,
			EnvVar: "GRPC_PROXY_UNARY_TIMEOUT",
		},
	)
}

type GRPCProxy struct {
	grpc         *grpc.Server
	claims       *Claims
	inited       bool
	mux          sync.Mutex
	logger       *logrus.Entry
	r            *Resolver
	src          *Source
	parser       *URLParser
	baseURL      string
	tries        int
	delay        int
	unaryTimeout int
}

func NewGRPCProxy(c *cli.Context, bu string, claims *Claims, r *Resolver, src *Source, parser *URLParser, logger *logrus.Entry) *GRPCProxy {
	return &GRPCProxy{
		baseURL:      bu,
		claims:       claims,
		r:            r,
		inited:       false,
		src:          src,
		logger:       logger,
		parser:       parser,
		tries:        c.Int(grpcProxyRedialTriesFlag),
		delay:        c.Int(grpcProxyRedialDelayFlag),
		unaryTimeout: c.Int(grpcProxyUnaryTimeoutFlag),
	}
}
func (s *GRPCProxy) dial(ctx context.Context, cl *Client, src *Source, opts []grpc.DialOption, invoke bool) (*grpc.ClientConn, error) {
	loc, err := s.r.Resolve(src, s.logger, false, invoke, cl)
	if err != nil {
		s.logger.WithError(err).Error("failed to get location")
		return nil, status.Errorf(codes.Unavailable, "Unavailable")
	}
	if loc.Unavailable {
		return nil, status.Errorf(codes.Unavailable, "Unavailable")
	}
	return grpc.DialContext(ctx, fmt.Sprintf("%s:%v", loc.IP.String(), loc.GRPC), opts...)
}

func (s *GRPCProxy) dialWithRetry(ctx context.Context, cl *Client, src *Source, opts []grpc.DialOption, invoke bool, tries int, delay int) (conn *grpc.ClientConn, err error) {
	for i := 0; i < tries; i++ {
		conn, err = s.dial(ctx, cl, src, opts, invoke)
		if err != nil {
			time.Sleep(time.Duration(delay) * time.Second)
		} else {
			break
		}
	}
	return
}
func unaryClientTimeoutInterceptor(t time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, cancel := context.WithTimeout(ctx, t)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (s *GRPCProxy) get() *grpc.Server {
	// logger := logrus.NewEntry(logrus.StandardLogger())
	// grpc.EnableTracing = true
	// grpc_logrus.ReplaceGrpcLogger(logger)

	// retryOpts := []grpcretry.CallOption{
	// 	grpcretry.WithPerRetryTimeout(3 * time.Second),
	// 	grpcretry.WithBackoff(grpcretry.BackoffLinear(500 * time.Millisecond)),
	// 	grpcretry.WithMax(3),
	// }
	grpcOpts := []grpc.DialOption{
		grpc.WithCodec(proxy.Codec()),
		grpc.WithInsecure(),
		// grpc.WithStreamInterceptor(grpcretry.StreamClientInterceptor(retryOpts...)),
		grpc.WithUnaryInterceptor(unaryClientTimeoutInterceptor(time.Duration(s.unaryTimeout) * time.Second)),
	}

	director := func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("token")) == 0 || md.Get("token")[0] == "" {
			return nil, nil, errors.Errorf("No token provided")
		}
		token := md.Get("token")[0]
		apiKey := ""
		if len(md.Get("api-key")) != 0 {
			apiKey = md.Get("api-key")[0]
		}
		path := ""
		if len(md.Get("path")) != 0 {
			path = md.Get("path")[0]
		}
		_, cl, err := s.claims.Get(md.Get("token")[0], apiKey)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to get claims")
		}
		src := s.src
		if path != "" && s.src == nil && s.parser != nil {
			nu, err := url.Parse(path)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "failed to parse url from path %v", path)
			}
			src, err = s.parser.Parse(nu)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "failed to parse path from metadata %v", path)
			}
		}
		if src == nil {
			return nil, nil, errors.Errorf("failed to find path")
		}

		invoke := true
		if len(md.Get("invoke")) != 0 && md.Get("invoke")[0] == "false" {
			invoke = false
		}
		mdCopy := md.Copy()
		mdCopy.Set("source-url", s.baseURL+"/"+src.InfoHash+src.Path+"?"+src.Query)
		mdCopy.Set("proxy-url", s.baseURL)
		mdCopy.Set("info-hash", src.InfoHash)
		mdCopy.Set("path", src.Path)
		mdCopy.Set("token", token)
		mdCopy.Set("api-key", apiKey)
		clientName := "default"
		if cl != nil {
			clientName = cl.Name
		}
		mdCopy.Set("client", clientName)
		delete(mdCopy, "user-agent")
		// If this header is present in the request from the web client,
		// the actual connection to the backend will not be established.
		// https://github.com/improbable-eng/grpc-web/issues/568
		delete(mdCopy, "connection")
		outCtx := metadata.NewOutgoingContext(ctx, mdCopy)
		conn, err := s.dialWithRetry(ctx, cl, src, grpcOpts, invoke, s.tries, s.delay)
		// conn, err := s.dial(ctx, cl, src, grpcOpts, invoke)
		return outCtx, conn, err
	}
	// Server with logging and monitoring enabled.
	g := grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()), // needed for proxy to function.
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpcMiddleware.WithUnaryServerChain(
		// grpc_logrus.UnaryServerInterceptor(logger),
		// grpc_prometheus.UnaryServerInterceptor,
		),
		grpcMiddleware.WithStreamServerChain(
		// grpc_logrus.StreamServerInterceptor(logger),
		// grpc_prometheus.StreamServerInterceptor,
		),
	)
	return g
}

func (s *GRPCProxy) Get() *grpc.Server {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.grpc
	}
	s.grpc = s.get()
	s.inited = true
	return s.grpc
}

func (s *GRPCProxy) Close() {
	if s.grpc == nil {
		return
	}
	s.grpc.Stop()
}
