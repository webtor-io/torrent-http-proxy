package services

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

type Web struct {
	host                string
	port                int
	ln                  net.Listener
	r                   *Resolver
	pr                  *HTTPProxyPool
	parser              *URLParser
	grpc                *HTTPGRPCProxyPool
	baseURL             string
	claims              *Claims
	k8s                 *K8SClient
	redirect            bool
	redirectPrefix      string
	redirectAddressType string
}

const (
	WEB_HOST                              = "host"
	WEB_PORT                              = "port"
	WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE = "origin-host-redirect-address-type"
	WEB_ORIGIN_HOST_REDIRECT              = "origin-host-redirect"
	WEB_ORIGIN_HOST_REDIRECT_PREFIX       = "origin-host-redirect-prefix"
)

var hexIPPattern = regexp.MustCompile(`[^\.]*`)

func NewWeb(c *cli.Context, baseURL string, parser *URLParser, r *Resolver, pr *HTTPProxyPool, grpc *HTTPGRPCProxyPool, claims *Claims, k8s *K8SClient) *Web {
	return &Web{host: c.String(WEB_HOST), port: c.Int(WEB_PORT),
		parser: parser, r: r, pr: pr, baseURL: baseURL, grpc: grpc, claims: claims, k8s: k8s,
		redirect:            c.Bool(WEB_ORIGIN_HOST_REDIRECT),
		redirectPrefix:      c.String(WEB_ORIGIN_HOST_REDIRECT_PREFIX),
		redirectAddressType: c.String(WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE)}
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
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   WEB_ORIGIN_HOST_REDIRECT,
		Usage:  "redirects requests to origin host",
		EnvVar: "WEB_ORIGIN_HOST_REDIRECT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   WEB_ORIGIN_HOST_REDIRECT_PREFIX,
		Usage:  "subdomain prefix of host to be redirected",
		EnvVar: "WEB_ORIGIN_HOST_REDIRECT_PREFIX",
		Value:  "abra--",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE,
		Usage:  "preferred node address type",
		EnvVar: "WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE",
		Value:  "ExternalIP",
	})
}

func (s *Web) getRedirectURL(r *http.Request, src *Source, logger *logrus.Entry, originalPath string, newPath string, invoke bool, cl *Client) (string, error) {
	if !s.redirect {
		return "", nil
	}
	h := r.Host
	parts := strings.Split(h, ":")
	// Skip internal requests
	if net.ParseIP(parts[0]) != nil {
		return "", nil
	}
	loc, err := s.r.Resolve(src, logger, false, invoke, cl)
	if err != nil {
		return "", errors.Wrap(err, "Failed to resolve location")
	}
	if loc.Unavailable || loc.HostIP == nil {
		return "", nil
	}
	u := r.URL
	ip := loc.HostIP
	hexIP := fmt.Sprintf("%02x%02x%02x%02x", ip[12], ip[13], ip[14], ip[15])
	if !strings.HasPrefix(h, s.redirectPrefix) {
		u := r.URL
		u.Host = s.redirectPrefix + hexIP + "." + h
	}
	u.Path = "/liveness"
	u.Scheme = "https"
	pu := u.String()
	resp, err := http.Get(pu)
	if err != nil || resp.StatusCode != http.StatusOK {
		u.Path = newPath
		logger.WithError(err).WithField("probe_url", pu).Error("Liveness probe is not OK")
		return "", nil
	}
	u.Path = originalPath
	return u.String(), nil
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, logger *logrus.Entry, originalPath string, newPath string) {
	apiKey := r.URL.Query().Get("api-key")
	claims, cl, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
	if err != nil {
		logger.WithError(err).Errorf("Failed to get claims")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	invoke := true
	if r.URL.Query().Get("invoke") == "false" {
		invoke = false
	}
	ru, err := s.getRedirectURL(r, src, logger, originalPath, newPath, invoke, cl)
	if err != nil {
		logger.WithError(err).Errorf("Failed to get redirect url")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if ru != "" {
		http.Redirect(w, r, ru, 302)
		return
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
	if s.redirect {
		logrus.Infof("Redirecting enabled with prefix=%s", s.redirectPrefix)
	}

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
		cl, err := s.k8s.Get()
		if err != nil {
			logrus.WithError(err).Error("Failed to get node client")
			w.WriteHeader(500)
			return
		}
		opts := metav1.ListOptions{}
		nodes, err := cl.CoreV1().Nodes().List(opts)
		if err != nil {
			logrus.WithError(err).Error("Failed to get node client")
			w.WriteHeader(500)
			return
		}
		res := []string{}
		for _, n := range nodes.Items {
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Status == corev1.ConditionTrue && c.Type == corev1.NodeReady {
					ready = true
				}
			}
			if !ready {
				continue
			}
			for _, a := range n.Status.Addresses {
				if a.Type == corev1.NodeAddressType(s.redirectAddressType) {
					byteIP := net.ParseIP(a.Address)
					hexIP := fmt.Sprintf("%02x%02x%02x%02x", byteIP[12], byteIP[13], byteIP[14], byteIP[15])
					res = append(res, s.redirectPrefix+hexIP)
				}
			}
		}
		json, err := json.Marshal(res)
		if err != nil {
			logrus.WithError(err).Error("Failed to get node client")
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
