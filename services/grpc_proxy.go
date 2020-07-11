package services

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"google.golang.org/grpc/codes"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
)

type GRPCProxy struct {
	grpc    *grpc.Server
	claims  *Claims
	inited  bool
	mux     sync.Mutex
	logger  *logrus.Entry
	r       *Resolver
	src     *Source
	parser  *URLParser
	baseURL string
}

func NewGRPCProxy(bu string, claims *Claims, r *Resolver, src *Source, parser *URLParser, logger *logrus.Entry) *GRPCProxy {
	return &GRPCProxy{baseURL: bu, claims: claims, r: r, inited: false, src: src, logger: logger, parser: parser}
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
		// grpc.WithUnaryInterceptor(grpcretry.UnaryClientInterceptor(retryOpts...)),
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
			return nil, nil, errors.Wrap(err, "Failed to get claims")
		}
		src := s.src
		if path != "" && s.src == nil && s.parser != nil {
			nu, err := url.Parse(path)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "Failed to parse url from path %v", path)
			}
			src, err = s.parser.Parse(nu)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "Failed to parse path from metadata %v", path)
			}
		}
		if src == nil {
			return nil, nil, errors.Errorf("Failed to find path")
		}

		invoke := true
		if len(md.Get("invoke")) != 0 && md.Get("invoke")[0] == "false" {
			invoke = false
		}
		outCtx, _ := context.WithCancel(ctx)
		mdCopy := md.Copy()
		md.Set("source-url", s.baseURL+"/"+src.InfoHash+src.Path+"?"+src.Query)
		md.Set("proxy-url", s.baseURL)
		md.Set("info-hash", src.InfoHash)
		md.Set("path", src.Path)
		md.Set("token", token)
		md.Set("api-key", apiKey)
		md.Set("client", cl.Name)
		delete(mdCopy, "user-agent")
		// If this header is present in the request from the web client,
		// the actual connection to the backend will not be established.
		// https://github.com/improbable-eng/grpc-web/issues/568
		delete(mdCopy, "connection")
		outCtx = metadata.NewOutgoingContext(outCtx, mdCopy)
		loc, err := s.r.Resolve(src, s.logger, false, invoke, cl)
		if err != nil {
			s.logger.WithError(err).Error("Failed to get location")
			return nil, nil, grpc.Errorf(codes.Unavailable, "Unavailable")
		}
		if loc.Unavailable {
			return nil, nil, grpc.Errorf(codes.Unavailable, "Unavailable")
		}
		conn, err := grpc.DialContext(ctx, fmt.Sprintf("%s:%v", loc.IP.String(), loc.GRPC), grpcOpts...)
		if err != nil {
			s.logger.Warn("Failed to dial location, try to refresh it")
			loc, err := s.r.Resolve(src, s.logger, true, invoke, cl)
			if err != nil {
				s.logger.WithError(err).Error("Failed to get new location")
				return nil, nil, err
			}
			conn, err = grpc.DialContext(ctx, fmt.Sprintf("%s:%v", loc.IP.String(), loc.GRPC), grpcOpts...)
			if err != nil {
				s.logger.WithError(err).Error("Failed to dial with new address")
				return nil, nil, err
			}
		}
		return outCtx, conn, err
	}
	// Server with logging and monitoring enabled.
	g := grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()), // needed for proxy to function.
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpc_middleware.WithUnaryServerChain(
		// grpc_logrus.UnaryServerInterceptor(logger),
		// grpc_prometheus.UnaryServerInterceptor,
		),
		grpc_middleware.WithStreamServerChain(
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
