package services

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

type SourceType string

const (
	Internal SourceType = "internal"
	External SourceType = "external"
)

type Web struct {
	host           string
	port           int
	ln             net.Listener
	r              *Resolver
	pr             *HTTPProxyPool
	parser         *URLParser
	grpc           *HTTPGRPCProxyPool
	subdomains     *SubdomainsPool
	bucketPool     *BucketPool
	clickHouse     *ClickHouse
	baseURL        string
	claims         *Claims
	cfg            *ConnectionsConfig
	ah             *AccessHistory
	bandwidthLimit bool
}

const (
	WEB_HOST            = "host"
	WEB_PORT            = "port"
	USE_BANDWIDTH_LIMIT = "use-bandwidth-limit"
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

var (
	promHTTPProxyRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "webtor_http_proxy_request_duration_seconds",
		Help: "HTTP Proxy request duration in seconds",
	}, []string{"source", "name", "status"})
	promHTTPProxyRequestTTFB = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "webtor_http_proxy_request_ttfb_seconds",
		Help: "HTTP Proxy request ttfb in seconds",
	}, []string{"source", "name", "status"})
	promHTTPProxyRequestSize = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_request_size_bytes",
		Help: "HTTP Proxy request size bytes",
	}, []string{"client", "domain", "role", "source", "name", "infohash", "file", "status"})
	promHTTPProxyRequestCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_request_current",
		Help: "HTTP Proxy request current",
	}, []string{"source", "name"})
	promHTTPProxyRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_request_total",
		Help: "HTTP Proxy dial total",
	}, []string{"source", "name", "infohash", "status"})
)

func init() {
	prometheus.MustRegister(promHTTPProxyRequestDuration)
	prometheus.MustRegister(promHTTPProxyRequestTTFB)
	prometheus.MustRegister(promHTTPProxyRequestSize)
	prometheus.MustRegister(promHTTPProxyRequestCurrent)
	prometheus.MustRegister(promHTTPProxyRequestTotal)
}

func NewWeb(c *cli.Context, baseURL string, parser *URLParser, r *Resolver, pr *HTTPProxyPool, grpc *HTTPGRPCProxyPool, claims *Claims, subs *SubdomainsPool, bp *BucketPool, ch *ClickHouse, cfg *ConnectionsConfig, ah *AccessHistory) *Web {
	return &Web{
		host:           c.String(WEB_HOST),
		port:           c.Int(WEB_PORT),
		parser:         parser,
		r:              r,
		pr:             pr,
		baseURL:        baseURL,
		grpc:           grpc,
		claims:         claims,
		subdomains:     subs,
		bucketPool:     bp,
		clickHouse:     ch,
		cfg:            cfg,
		ah:             ah,
		bandwidthLimit: c.Bool(USE_BANDWIDTH_LIMIT),
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
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   USE_BANDWIDTH_LIMIT,
		Usage:  "use bandwidth limit",
		EnvVar: "USE_BANDWIDTH_LIMIT",
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
		return strings.Split(forwarded, ",")[0]
	}
	return r.RemoteAddr
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, logger *logrus.Entry, originalPath string, newPath string) {
	wi := NewResponseWrtierInterceptor(w)
	w = wi
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
		logger.WithError(err).Warnf("Failed to get claims")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	source := Internal
	if r.Header.Get("X-FORWARDED-FOR") != "" {
		source = External
		remoteAddress, raOK := claims["remoteAddress"].(string)
		ua, uaOK := claims["agent"].(string)
		if raOK && uaOK && (s.getIP(r) != remoteAddress || r.Header.Get("User-Agent") != ua) {
			ok, left := s.ah.Store(remoteAddress, ua, s.getIP(r), r.Header.Get("User-Agent"))
			logger.Warningf("IP or UA changed got ua=%v ip=%v x-forwarded-for=%v, expected ua=%v ip=%v, changes left=%v, access=%v",
				r.Header.Get("User-Agent"), s.getIP(r), r.Header.Get("X-FORWARDED-FOR"), ua, remoteAddress, left, ok)
			if !ok {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
	}

	ads := false
	invoke := true
	if r.URL.Query().Get("invoke") == "false" {
		invoke = false
	}
	clientName := "default"
	if cl != nil {
		clientName = cl.Name
	}
	role := "nobody"
	if r, ok := claims["role"].(string); ok {
		role = r
	}
	if r, ok := claims["ads"].(bool); ok {
		ads = r
	}
	domain := "default"
	if d, ok := claims["domain"].(string); ok {
		domain = d
	}

	sessionID := ""
	if sid, ok := claims["sessionID"].(string); ok {
		sessionID = sid
	}

	promHTTPProxyRequestCurrent.WithLabelValues(string(source), src.GetEdgeName()).Inc()
	defer func() {
		if s.clickHouse != nil && wi.bytesWritten > 0 && wi.GroupedStatusCode() == 200 {
			err := s.clickHouse.Add(&StatRecord{
				ApiKey:        apiKey,
				BytesWritten:  uint64(wi.bytesWritten),
				Client:        clientName,
				Domain:        domain,
				Duration:      uint64(time.Since(wi.start).Milliseconds()),
				Edge:          src.GetEdgeName(),
				GroupedStatus: uint64(wi.GroupedStatusCode()),
				InfoHash:      src.InfoHash,
				OriginalPath:  src.OriginPath,
				Path:          src.Path,
				Role:          role,
				SessionID:     sessionID,
				Source:        string(source),
				Status:        uint64(wi.statusCode),
				TTFB:          uint64(wi.ttfb.Milliseconds()),
				Timestamp:     time.Now(),
				Ads:           ads,
			})
			if err != nil {
				logger.WithError(err).Warn("Failed to store data to ClickHouse")
			}
		}
		promHTTPProxyRequestDuration.WithLabelValues(string(source), src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(time.Since(wi.start).Seconds())
		if wi.bytesWritten > 0 {
			promHTTPProxyRequestTTFB.WithLabelValues(string(source), src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(wi.ttfb.Seconds())
		}
		promHTTPProxyRequestCurrent.WithLabelValues(string(source), src.GetEdgeName()).Dec()
		promHTTPProxyRequestTotal.WithLabelValues(string(source), src.GetEdgeName(), src.InfoHash, strconv.Itoa(wi.GroupedStatusCode())).Inc()
		promHTTPProxyRequestSize.WithLabelValues(
			clientName,
			domain,
			role,
			string(source),
			src.GetEdgeName(),
			src.InfoHash,
			src.Path,
			strconv.Itoa(wi.GroupedStatusCode()),
		).Add(float64(wi.bytesWritten))
		rate, _ := claims["rate"].(string)
		l := logger.WithFields(logrus.Fields{
			"client":     clientName,
			"domain":     domain,
			"role":       role,
			"source":     string(source),
			"edge":       src.GetEdgeName(),
			"infohash":   src.InfoHash,
			"path":       src.Path,
			"ttfb":       wi.ttfb.Seconds(),
			"duration":   time.Since(wi.start).Seconds(),
			"status":     strconv.Itoa(wi.statusCode),
			"rate":       rate,
			"session_id": sessionID,
			"referer":    r.Referer(),
		})
		if wi.GroupedStatusCode() == 500 {
			l.Error("Failed to serve request")
		} else if wi.GroupedStatusCode() == 200 {
			l.Info("Request served successfully")
		} else {
			l.Warn("Bad request")
		}
	}()

	headers := map[string]string{
		"X-Source-Url":  s.baseURL + "/" + src.InfoHash + src.Path + "?" + src.Query,
		"X-Proxy-Url":   s.baseURL,
		"X-Info-Hash":   src.InfoHash,
		"X-Path":        src.Path,
		"X-Origin-Path": src.OriginPath,
		"X-Full-Path":   "/" + src.InfoHash + "/" + url.PathEscape(strings.TrimPrefix(src.Path, "/")),
		"X-Token":       src.Token,
		"X-Api-Key":     apiKey,
		"X-Client":      clientName,
		"X-Session-ID":  sessionID,
	}

	rate, ok := claims["rate"].(string)
	if ok {
		headers["X-Download-Rate"] = rate
	}

	cfg := s.cfg.GetMod(src.GetEdgeType())

	if cfg.Headers != nil {
		for k, v := range cfg.Headers {
			headers[k] = v
		}
	}

	if s.bandwidthLimit && source == External {
		b, err := s.bucketPool.Get(claims)
		if err != nil {
			logger.WithError(err).Errorf("Failed to get bucket")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if b != nil {
			w = NewThrottledRequestWrtier(w, b)
		}
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
	mux.HandleFunc("/subdomains.json", func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.URL.Query().Get("api-key")
		_, _, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to get claims")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		pool := r.URL.Query().Get("pool")
		pools := strings.Split(pool, ",")
		if len(pools) == 0 {
			pools = []string{"worker"}
		}
		if len(pools) == 1 && pools[0] == "any" {
			pools = []string{}
		}
		sc, subs, err := s.subdomains.Get(
			r.URL.Query().Get("infohash"),
			r.URL.Query().Get("skip-active-job-search") == "true",
			r.URL.Query().Get("use-cpu") == "true",
			r.URL.Query().Get("use-bandwidth") == "true",
			pools,
		)
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
		if r.URL.Query().Get("debug") == "true" {
			w.Write([]byte(fmt.Sprintf("%+v", sc)))
		} else {
			w.Write(json)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" ||
			strings.HasPrefix(r.URL.Path, "/favicon") ||
			strings.HasPrefix(r.URL.Path, "/ads.txt") ||
			strings.HasPrefix(r.URL.Path, "/robots.txt") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(200)
			return
		}
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
		// if f, ok := w.(http.Flusher); ok {
		// 	f.Flush()
		// }
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

		s.proxyHTTP(w, r, src, logger, originalPath, newPath)

	})
	logrus.Infof("Serving Web at %v", addr)
	srv := &http.Server{
		Handler: mux,
		// ReadTimeout:    5 * time.Minute,
		// WriteTimeout:   5 * time.Minute,
		MaxHeaderBytes: 50 << 20,
	}
	return srv.Serve(ln)
}

func (s *Web) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}
