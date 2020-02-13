package services

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"google.golang.org/grpc/codes"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
)

type GRPCProxy struct {
	grpc   *grpcweb.WrappedGrpcServer
	claims *Claims
	inited bool
	mux    sync.Mutex
	logger *logrus.Entry
	r      *Resolver
	src    *Source
}

func NewGRPCProxy(claims *Claims, r *Resolver, src *Source, logger *logrus.Entry) *GRPCProxy {
	return &GRPCProxy{claims: claims, r: r, inited: false, src: src, logger: logger}
}

func (s *GRPCProxy) get() *grpcweb.WrappedGrpcServer {
	// logger := logrus.NewEntry(logrus.StandardLogger())
	// grpc.EnableTracing = true
	// grpc_logrus.ReplaceGrpcLogger(logger)

	director := func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("token")) == 0 || md.Get("token")[0] == "" {
			return nil, nil, errors.Errorf("No token provided")
		}
		apiKey := ""
		if len(md.Get("api-key")) != 0 {
			apiKey = md.Get("api-key")[0]
		}
		_, cl, err := s.claims.Get(md.Get("token")[0], apiKey)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed to get claims")
		}

		invoke := true
		if len(md.Get("invoke")) != 0 && md.Get("invoke")[0] == "false" {
			invoke = false
		}
		outCtx, _ := context.WithCancel(ctx)
		mdCopy := md.Copy()
		delete(mdCopy, "user-agent")
		// If this header is present in the request from the web client,
		// the actual connection to the backend will not be established.
		// https://github.com/improbable-eng/grpc-web/issues/568
		delete(mdCopy, "connection")
		outCtx = metadata.NewOutgoingContext(outCtx, mdCopy)
		loc, err := s.r.Resolve(s.src, s.logger, false, invoke, cl)
		if err != nil {
			s.logger.WithError(err).Error("Failed to get location")
			return nil, nil, grpc.Errorf(codes.Unavailable, "Unavailable")
		}
		if loc.Unavailable {
			return nil, nil, grpc.Errorf(codes.Unavailable, "Unavailable")
		}
		conn, err := grpc.DialContext(ctx, fmt.Sprintf("%s:%v", loc.IP.String(), loc.GRPC),
			grpc.WithCodec(proxy.Codec()), grpc.WithInsecure())
		if err != nil {
			s.logger.Warn("Failed to dial location, try to refresh it")
			loc, err := s.r.Resolve(s.src, s.logger, true, invoke, cl)
			if err != nil {
				s.logger.WithError(err).Error("Failed to get new location")
				return nil, nil, err
			}
			conn, err = grpc.DialContext(ctx, fmt.Sprintf("%s:%v", loc.IP.String(), loc.GRPC),
				grpc.WithCodec(proxy.Codec()), grpc.WithInsecure())
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
	w := grpcweb.WrapServer(g,
		grpcweb.WithWebsockets(true),
		grpcweb.WithOriginFunc(makeHttpOriginFunc()),
		grpcweb.WithWebsocketOriginFunc(makeWebsocketOriginFunc()),
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
	)
	return w
}

func (s *GRPCProxy) Get() *grpcweb.WrappedGrpcServer {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.grpc
	}
	s.grpc = s.get()
	s.inited = true
	return s.grpc
}

func makeHttpOriginFunc() func(origin string) bool {
	return func(origin string) bool {
		return true
	}
}
func makeWebsocketOriginFunc() func(req *http.Request) bool {
	return func(req *http.Request) bool {
		return true
	}
}
