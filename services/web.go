package services

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

type Web struct {
	host       string
	port       int
	ln         net.Listener
	r          *Resolver
	pr         *HTTPProxyPool
	parser     *URLParser
	grpc       *HTTPGRPCProxyPool
	subdomains *SubdomainsPool
	baseURL    string
	claims     *Claims
}

const (
	WEB_HOST = "host"
	WEB_PORT = "port"
)

var (
	allowList = []string{
		"/s-1-v1-a1.ts",
		"/s-2-v1-a1.ts",
		"/s-3-v1-a1.ts",
		"/s-4-v1-a1.ts",
		"/s-5-v1-a1.ts",
		"/s-6-v1-a1.ts",
		"/s-7-v1-a1.ts",
		"/s-8-v1-a1.ts",
		"/s-9-v1-a1.ts",
		"/s-10-v1-a1.ts",
		".png",
		".gif",
		".jpg",
		".jpeg",
	}
)

func NewWeb(c *cli.Context, baseURL string, parser *URLParser, r *Resolver, pr *HTTPProxyPool, grpc *HTTPGRPCProxyPool, claims *Claims, subs *SubdomainsPool) *Web {
	return &Web{host: c.String(WEB_HOST), port: c.Int(WEB_PORT),
		parser: parser, r: r, pr: pr, baseURL: baseURL, grpc: grpc, claims: claims,
		subdomains: subs,
	}
}

func RegisterWebFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  WEB_HOST,
		Usage: "listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  WEB_PORT,
		Usage: "http listening port",
		Value: 8080,
	})
}

func isAllowed(r *http.Request) bool {
	for _, v := range allowList {
		if strings.HasSuffix(r.URL.Path, v) {
			return true
		}
	}
	return false
}

func (s *Web) getIP(r *http.Request) string {
	forwarded := r.Header.Get("X-FORWARDED-FOR")
	if forwarded != "" {
		return forwarded
	}
	return r.RemoteAddr
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, logger *logrus.Entry, originalPath string, newPath string) {
	apiKey := r.URL.Query().Get("api-key")
	if r.URL.Query().Get("token") == "" && (r.Header.Get("X-FORWARDED-FOR") == "" || isAllowed(r)) {
		token, err := s.claims.Set(apiKey, StandardClaims{})
		if err != nil {
			logger.WithError(err).Errorf("Failed to set claims")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		logger.Info(token)
		if token != "" {
			q, _ := url.ParseQuery(r.URL.RawQuery)
			q.Add("token", token)
			r.URL.RawQuery = q.Encode()
		}
		logger.Infof("Got allowed request %v", r.URL.Path)
	}
	claims, cl, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
	if err != nil {
		logger.WithError(err).Errorf("Failed to get claims")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.Header.Get("X-FORWARDED-FOR") != "" {
		remoteAddress, raOK := claims["remoteAddress"].(string)
		ua, uaOK := claims["agent"].(string)
		if raOK && uaOK && s.getIP(r) != remoteAddress && r.Header.Get("User-Agent") != ua {
			logger.Warningf("IP and UA changed, got ua=%v ip=%v, expected ua=%v ip=%v so deny access",
				r.Header.Get("User-Agent"), s.getIP(r), ua, remoteAddress)
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	invoke := true
	if r.URL.Query().Get("invoke") == "false" {
		invoke = false
	}

	clientName := "default"
	if cl != nil {
		clientName = cl.Name
	}
	headers := map[string]string{
		"X-Source-Url": s.baseURL + "/" + src.InfoHash + src.Path + "?" + src.Query,
		"X-Proxy-Url":  s.baseURL,
		"X-Info-Hash":  src.InfoHash,
		"X-Path":       src.Path,
		"X-Full-Path":  "/" + src.InfoHash + "/" + url.PathEscape(strings.TrimPrefix(src.Path, "/")),
		"X-Token":      src.Token,
		"X-Api-Key":    apiKey,
		"X-Client":     clientName,
	}
	rate, ok := claims["rate"].(string)
	if ok {
		headers["X-Download-Rate"] = rate
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	pr, err := s.pr.Get(src, logger, invoke, cl)

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

	var ip net.IP
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
		}
	}

	mux.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Current ip:\t%v\n", ip.String())
		fmt.Fprintf(w, "Remote addr:\t%v\n", r.RemoteAddr)
	})
	mux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/subdomains.json", func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.URL.Query().Get("api-key")
		_, _, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to get claims")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		subs, err := s.subdomains.Get(r.URL.Query().Get("infohash"))
		if err != nil {
			logrus.WithError(err).Error("Failed to get subdomains")
			w.WriteHeader(500)
			return
		}
		json, err := json.Marshal(subs)
		if err != nil {
			logrus.WithError(err).Error("Failed to marshal subdomains")
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		w.Write(json)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger := logrus.WithFields(logrus.Fields{
			"URL":  r.URL.String(),
			"Host": r.Host,
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

		// if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		// w.Header().Set("Access-Control-Allow-Credentials", "true")
		// }

		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Download-Id, User-Id, Token, X-Grpc-Web, Api-Key, Range")
			w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,OPTIONS,POST,PUT")
			w.Header().Set("Access-Control-Max-Age", "600")
			return
		}

		originalPath := r.URL.Path

		newPath := ""

		if src.Mod != nil {
			newPath = src.Mod.Path
		} else {
			newPath = src.Path
		}
		r.URL.Path = newPath

		ws, err := s.grpc.Get(src, logger)

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
		s.proxyHTTP(w, r, src, logger, originalPath, newPath)

	})
	logrus.Infof("Serving Web at %v", addr)
	return http.Serve(ln, mux)
}

func (s *Web) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
