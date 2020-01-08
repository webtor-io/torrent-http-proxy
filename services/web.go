package services

import (
	"fmt"
	"net"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

type Web struct {
	host    string
	port    int
	ln      net.Listener
	r       *Resolver
	pr      *HTTPProxyPool
	parser  *URLParser
	grpc    *GRPCProxyPool
	baseURL string
	claims  *Claims
}

const (
	WEB_HOST_FLAG = "host"
	WEB_PORT_FLAG = "port"
)

func NewWeb(c *cli.Context, baseURL string, parser *URLParser, r *Resolver, pr *HTTPProxyPool, grpc *GRPCProxyPool, claims *Claims) *Web {
	return &Web{host: c.String(WEB_HOST_FLAG), port: c.Int(WEB_PORT_FLAG),
		parser: parser, r: r, pr: pr, baseURL: baseURL, grpc: grpc, claims: claims}
}

func RegisterWebFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  WEB_HOST_FLAG,
		Usage: "listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  WEB_PORT_FLAG,
		Usage: "http listening port",
		Value: 8080,
	})
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, locw *LocationWrapper, logger *logrus.Entry) {
	claims, err := s.claims.Get(r.URL.Query().Get("token"))
	if err != nil {
		logger.WithError(err).Errorf("Failed to get claims")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	headers := map[string]string{
		"X-Source-Url": s.baseURL + "/" + src.InfoHash + src.Path + "?token=" + src.Token,
		"X-Proxy-Url":  s.baseURL,
		"X-Info-Hash":  src.InfoHash,
		"X-Path":       src.Path,
		"X-Token":      src.Token,
	}
	rate, ok := claims["rate"].(string)
	if ok {
		headers["X-Download-Rate"] = rate
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	pr, err := s.pr.Get(locw, logger)

	if err != nil {
		logger.WithError(err).Errorf("Failed to get proxy")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if pr == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	pr.ServeHTTP(w, r)
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to web listen to tcp connection")
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger := logrus.WithFields(logrus.Fields{
			"URL": r.URL.String(),
		})

		src, err := s.parser.Parse(r.URL)

		if err != nil {
			logger.WithError(err).Error("Failed to parse url")
			w.WriteHeader(500)
			return
		}

		logger = logger.WithFields(logrus.Fields{
			"InfoHash": src.InfoHash,
			"Path":     src.Path,
		})

		if r.Header.Get("Origin") != "" {
			w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Download-Id, User-Id,, Token, X-Grpc-Web")
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Max-Age", "600")
			return
		}

		locw := NewLocationWrapper(src, s.r)

		if src.Mod != nil {
			r.URL.Path = src.Mod.Path
		} else {
			r.URL.Path = src.Path
		}

		ws, err := s.grpc.Get(locw, logger)

		if err != nil {
			logger.WithError(err).Error("Failed to get GRPC proxy")
			w.WriteHeader(500)
			return
		}

		if ws != nil {
			if ws.IsGrpcWebRequest(r) {
				logger.Info("Handling GRPC Web Request")
				ws.HandleGrpcWebRequest(w, r)
				return
			}
			if ws.IsGrpcWebSocketRequest(r) {
				logger.Info("Handling GRPC WebSocket Request")
				ws.HandleGrpcWebsocketRequest(w, r)
				return
			}
		}

		logger.Info("Handling HTTP")
		s.proxyHTTP(w, r, src, locw, logger)

	})
	logrus.Infof("Serving Web at %v", addr)
	return http.Serve(ln, mux)
}

func (s *Web) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
