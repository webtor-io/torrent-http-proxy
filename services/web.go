package services

import (
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
	pr             *HTTPProxy
	parser         *URLParser
	bucket         *Bucket
	clickHouse     *ClickHouse
	baseURL        string
	claims         *Claims
	cfg            *ServicesConfig
	ah             *AccessHistory
	bandwidthLimit bool
}

const (
	webHostFlag           = "host"
	webPortFlag           = "port"
	useBandwidthLimitFlag = "use-bandwidth-limit"
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
	}, []string{"domain", "role", "source", "name", "infohash", "file", "status"})
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

func NewWeb(c *cli.Context, parser *URLParser, r *Resolver, pr *HTTPProxy, claims *Claims, bp *Bucket, ch *ClickHouse, cfg *ServicesConfig, ah *AccessHistory) *Web {
	return &Web{
		host:           c.String(webHostFlag),
		port:           c.Int(webPortFlag),
		parser:         parser,
		r:              r,
		pr:             pr,
		claims:         claims,
		bucket:         bp,
		clickHouse:     ch,
		cfg:            cfg,
		ah:             ah,
		bandwidthLimit: c.Bool(useBandwidthLimitFlag),
	}
}

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:  webHostFlag,
			Usage: "listening host",
			Value: "",
		},
		cli.IntFlag{
			Name:  webPortFlag,
			Usage: "http listening port",
			Value: 8080,
		},
		cli.BoolFlag{
			Name:   useBandwidthLimitFlag,
			Usage:  "use bandwidth limit",
			EnvVar: "USE_BANDWIDTH_LIMIT",
		},
	)
}

func (s *Web) getIP(r *http.Request) string {
	forwarded := r.Header.Get("X-FORWARDED-FOR")
	if forwarded != "" {
		return strings.Split(forwarded, ",")[0]
	}
	return r.RemoteAddr
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, logger *logrus.Entry) {
	wi := NewResponseWrtierInterceptor(w)
	w = wi
	apiKey := r.URL.Query().Get("api-key")
	claims, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
	if err != nil {
		logger.WithError(err).Warnf("failed to get claims")
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
				logger.WithError(err).Warn("failed to store data to ClickHouse")
			}
		}
		promHTTPProxyRequestDuration.WithLabelValues(string(source), src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(time.Since(wi.start).Seconds())
		if wi.bytesWritten > 0 {
			promHTTPProxyRequestTTFB.WithLabelValues(string(source), src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(wi.ttfb.Seconds())
		}
		promHTTPProxyRequestCurrent.WithLabelValues(string(source), src.GetEdgeName()).Dec()
		promHTTPProxyRequestTotal.WithLabelValues(string(source), src.GetEdgeName(), src.InfoHash, strconv.Itoa(wi.GroupedStatusCode())).Inc()
		promHTTPProxyRequestSize.WithLabelValues(
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
			l.Error("failed to serve request")
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
		b, err := s.bucket.Get(claims)
		if err != nil {
			logger.WithError(err).Errorf("failed to get bucket")
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

	pr, err := s.pr.Get(r.Context(), src, logger)

	if err != nil {
		logger.WithError(err).Errorf("failed to get proxy")
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
		return errors.Wrap(err, "failed to web listen to tcp connection")
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
		_, _ = fmt.Fprintf(w, "Current ip:\t%v\n", ip.String())
		_, _ = fmt.Fprintf(w, "Remote addr:\t%v\n", r.RemoteAddr)
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
			logger.WithError(err).Error("failed to parse url")
			w.WriteHeader(500)
			return
		}

		logger = logger.WithFields(logrus.Fields{
			"InfoHash": src.InfoHash,
			"Path":     src.Path,
		})

		w.Header().Set("Access-Control-Allow-Origin", "*")

		newPath := ""

		if src.Mod != nil {
			newPath = src.Mod.Path
		} else {
			newPath = src.Path
		}
		r.URL.Path = newPath

		s.proxyHTTP(w, r, src, logger)

	})
	logrus.Infof("serving Web at %v", addr)
	srv := &http.Server{
		Handler:        mux,
		MaxHeaderBytes: 50 << 20,
	}
	return srv.Serve(ln)
}

func (s *Web) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
