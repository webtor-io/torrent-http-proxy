package services

import (
	"net/http"
	"sync"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
)

type HTTPGRPCProxy struct {
	v      *grpcweb.WrappedGrpcServer
	p      *GRPCProxy
	inited bool
	mux    sync.Mutex
}

func NewHTTPGRPCProxy(p *GRPCProxy) *HTTPGRPCProxy {
	return &HTTPGRPCProxy{p: p}
}

func (s *HTTPGRPCProxy) get() *grpcweb.WrappedGrpcServer {
	g := s.p.Get()
	w := grpcweb.WrapServer(g,
		grpcweb.WithWebsockets(true),
		grpcweb.WithWebsocketPingInterval(30*time.Second),
		grpcweb.WithOriginFunc(makeHttpOriginFunc()),
		grpcweb.WithWebsocketOriginFunc(makeWebsocketOriginFunc()),
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
	)
	return w
}

func (s *HTTPGRPCProxy) Get() *grpcweb.WrappedGrpcServer {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.v
	}
	s.v = s.get()
	s.inited = true
	return s.v
}

func (s *HTTPGRPCProxy) Close() {
	if s.v == nil {
		return
	}
	s.p.Close()
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
