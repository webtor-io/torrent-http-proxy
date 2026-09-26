package services

import (
	"context"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

type SourceType string

const (
	Internal SourceType = "internal"
	External SourceType = "external"
)

type Web struct {
	host             string
	port             int
	ln               net.Listener
	r                *Resolver
	pr               *HTTPProxy
	parser           *URLParser
	bucket           *HybridBucketPool
	clickHouse       *ClickHouse
	baseURL          string
	claims           *Claims
	ah               *AccessHistory
	bandwidthLimit   bool
	sl               *SessionLimiter
	enforceSessionIP bool
	stats            *SessionStats
	// gs drains in-flight requests on Close, up to the shutdown timeout
	// (WEB_SHUTDOWN_TIMEOUT), instead of dropping them with the listener.
	gs *cs.GracefulServer
	// closing is closed first thing in Close: responses that never end on
	// their own (session-stats streams, the seeder's ?stats streams) end on
	// it instead of holding up the drain.
	closing   chan struct{}
	closeOnce sync.Once
}

const (
	webHostFlag              = "host"
	webPortFlag              = "port"
	torrentHTTPProxyHostFlag = "torrent-http-proxy-host"
	torrentHTTPProxyPortFlag = "torrent-http-proxy-port"
	useBandwidthLimitFlag    = "use-bandwidth-limit"
	enforceSessionIPFlag     = "enforce-session-ip"
)

// A 2xx response enters webtor_http_proxy_throttle_ratio only when it is at
// least throttleRatioMinBytes and at least throttleRatioMinRateSeconds' worth
// of the token's rate. A session's bucket holds one second of the rate
// (capacity == rate), and a Redis-backed one keeps up to another second
// prefetched locally, so up to ~2 s of the rate leaves without any wait:
// ~12 MiB at 50M, most HLS segments. Such a response reads ratio ≈ 0 whether
// the upstream was slow or fast, which says nothing about tier vs upstream.
// Four seconds leaves at least half of a tier-bound response to the limiter.
// The byte floor keeps manifests, subtitles and probes out at rates under 2M.
const (
	throttleRatioMinBytes       = 1 << 20
	throttleRatioMinRateSeconds = 4
)

var (
	promHTTPProxyRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "webtor_http_proxy_request_duration_seconds",
		Help: "HTTP Proxy request duration in seconds",
	}, []string{"source", "role", "name", "status"})
	promHTTPProxyRequestTTFB = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "webtor_http_proxy_request_ttfb_seconds",
		Help: "HTTP Proxy request ttfb in seconds",
	}, []string{"source", "role", "name", "status"})
	promHTTPProxyRequestSize = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_request_size_bytes",
		Help: "HTTP Proxy request size bytes",
	}, []string{"domain", "role", "source", "name", "status"})
	promHTTPProxyRequestCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_request_current",
		Help: "HTTP Proxy request current",
	}, []string{"source", "role", "name"})
	promHTTPProxyRequestTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_request_total",
		Help: "HTTP Proxy dial total",
	}, []string{"source", "role", "name", "status"})
	// The three throttle counters cover one population: responses a limiter
	// was installed on, minus event streams. Per response, wait and
	// downstream are parts of duration, so wait/duration and
	// downstream/duration are time-weighted shares of that population;
	// request_duration_seconds_sum also holds limiter-less requests and SSE.
	// Labels match webtor_http_proxy_request_duration_seconds minus status.
	promHTTPProxyThrottleWait = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_throttle_wait_seconds_total",
		Help: "HTTP Proxy time throttled responses spent blocked in the bandwidth limiter in seconds",
	}, []string{"source", "role", "name"})
	promHTTPProxyThrottledDuration = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_throttled_request_duration_seconds_total",
		Help: "HTTP Proxy duration of throttled responses in seconds",
	}, []string{"source", "role", "name"})
	promHTTPProxyThrottledDownstream = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_throttled_request_downstream_seconds_total",
		Help: "HTTP Proxy time throttled responses spent blocked writing downstream in seconds",
	}, []string{"source", "role", "name"})
	promHTTPProxyThrottleRatio = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "webtor_http_proxy_throttle_ratio",
		Help:    "HTTP Proxy share of response duration spent blocked in the bandwidth limiter (2xx of at least 1 MiB and 4 s of the rate)",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99},
	}, []string{"role", "name", "download"})
)

func init() {
	prometheus.MustRegister(promHTTPProxyRequestDuration)
	prometheus.MustRegister(promHTTPProxyRequestTTFB)
	prometheus.MustRegister(promHTTPProxyRequestSize)
	prometheus.MustRegister(promHTTPProxyRequestCurrent)
	prometheus.MustRegister(promHTTPProxyRequestTotal)
	prometheus.MustRegister(promHTTPProxyThrottleWait)
	prometheus.MustRegister(promHTTPProxyThrottledDuration)
	prometheus.MustRegister(promHTTPProxyThrottledDownstream)
	prometheus.MustRegister(promHTTPProxyThrottleRatio)
}

func NewWeb(c *cli.Context, parser *URLParser, r *Resolver, pr *HTTPProxy, claims *Claims, bp *HybridBucketPool, ch *ClickHouse, ah *AccessHistory, sl *SessionLimiter) *Web {
	return &Web{
		host:             c.String(webHostFlag),
		port:             c.Int(webPortFlag),
		baseURL:          fmt.Sprintf("http://%s:%d", c.String(torrentHTTPProxyHostFlag), c.Int(torrentHTTPProxyPortFlag)),
		parser:           parser,
		r:                r,
		pr:               pr,
		claims:           claims,
		bucket:           bp,
		clickHouse:       ch,
		ah:               ah,
		bandwidthLimit:   c.Bool(useBandwidthLimitFlag),
		sl:               sl,
		enforceSessionIP: c.Bool(enforceSessionIPFlag),
		stats:            NewSessionStats(),
		closing:          make(chan struct{}),
		gs:               cs.NewGracefulServer(cs.ShutdownTimeout(c)),
	}
}

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   webHostFlag,
			Usage:  "listening host",
			Value:  "",
			EnvVar: "WEB_HOST",
		},
		cli.IntFlag{
			Name:   webPortFlag,
			Usage:  "http listening port",
			Value:  8080,
			EnvVar: "WEB_PORT",
		},
		cli.StringFlag{
			Name:   torrentHTTPProxyHostFlag,
			Usage:  "torrent http proxy host",
			EnvVar: "TORRENT_HTTP_PROXY_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   torrentHTTPProxyPortFlag,
			Usage:  "torrent http proxy port",
			Value:  8080,
			EnvVar: "TORRENT_HTTP_PROXY_SERVICE_PORT",
		},
		cli.BoolFlag{
			Name:   useBandwidthLimitFlag,
			Usage:  "use bandwidth limit",
			EnvVar: "USE_BANDWIDTH_LIMIT",
		},
		cli.BoolTFlag{
			Name:   enforceSessionIPFlag,
			Usage:  "reject requests whose client IP doesn't match the remoteAddress claim in the JWT (normalized to /24 for v4, /64 for v6). Disable to unblock mobile users if false positives appear.",
			EnvVar: "ENFORCE_SESSION_IP",
		},
	)
}

// sameSubnet reports whether two IP strings share the same subnet prefix
// (/24 for IPv4, /64 for IPv6). Returns true when either side is unparseable
// so we fail open rather than 429 a legitimate user on a malformed claim.
func sameSubnet(a, b string) bool {
	ipA := parseClientIP(a)
	ipB := parseClientIP(b)
	if ipA == nil || ipB == nil {
		return true
	}
	if v4a, v4b := ipA.To4(), ipB.To4(); v4a != nil && v4b != nil {
		return v4a.Mask(net.CIDRMask(24, 32)).Equal(v4b.Mask(net.CIDRMask(24, 32)))
	}
	if ipA.To4() != nil || ipB.To4() != nil {
		return false // one is v4, other is v6
	}
	return ipA.Mask(net.CIDRMask(64, 128)).Equal(ipB.Mask(net.CIDRMask(64, 128)))
}

func parseClientIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	return net.ParseIP(s)
}

// escapePathSegments percent-encodes every segment of a decoded path so it
// can be glued into a URL. src.Path is url.URL.Path — already decoded — so a
// file name containing '#', '?' or '%' must be re-encoded before it is
// handed to services that parse X-Source-Url as a URL (content-prober's
// ffprobe, content-transcoder's ffmpeg, srt2vtt, video-info): an unescaped
// '#' cuts the path at the fragment and the seeder is asked for a file that
// does not exist. Slashes are kept, everything else follows url.PathEscape.
// Seen with "Celeb vagyok… - S02 - 2008#10#27 - ….mkv" (2026-09-18): 118
// "probing failed" over 51 torrents in a week were this.
func escapePathSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// throttleRatio is the share of total that was spent blocked in the limiter,
// clamped to [0, 1].
func throttleRatio(waited, total time.Duration) float64 {
	if total <= 0 {
		return 0
	}
	return math.Min(math.Max(waited.Seconds()/total.Seconds(), 0), 1)
}

// throttleRatioMinSize is the smallest response, in bytes, whose throttle
// ratio is observed under the token rate; an unparsable rate observes none.
func throttleRatioMinSize(rate string) int {
	bytesPerSec, err := rateBytesPerSec(rate)
	if err != nil {
		return math.MaxInt
	}
	return max(throttleRatioMinBytes, int(math.Ceil(throttleRatioMinRateSeconds*bytesPerSec)))
}

// isEventStream reports whether the response is Server-Sent Events, the way
// ReverseProxy recognises it for immediate flushing.
func isEventStream(h http.Header) bool {
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	return mt == "text/event-stream"
}

func (s *Web) getIP(r *http.Request) string {
	forwarded := r.Header.Get("X-FORWARDED-FOR")
	if forwarded != "" {
		return strings.Split(forwarded, ",")[0]
	}
	return r.RemoteAddr
}

// setModHeaders sets X-Mod-Type and X-Mod-Extra in headers unconditionally:
// to the mod's values when src.Mod != nil, and to the empty string
// otherwise. Called unconditionally (not just when a mod is present) so a
// client-supplied X-Mod-Extra never reaches a downstream service.
func setModHeaders(headers map[string]string, src *Source) {
	headers["X-Mod-Type"] = ""
	headers["X-Mod-Extra"] = ""
	if src.Mod != nil {
		headers["X-Mod-Type"] = src.Mod.Type
		headers["X-Mod-Extra"] = src.Mod.Extra
	}
}

func (s *Web) proxyHTTP(w http.ResponseWriter, r *http.Request, src *Source, logger *logrus.Entry) {
	wi := NewResponseWrtierInterceptor(w)
	w = wi
	// Taken here, before anything derives r's context: errorHandler tells a
	// client that left from thp's own cancellations by this one.
	outcome := &proxyOutcome{client: r.Context()}
	apiKey := r.URL.Query().Get("api-key")
	claims, err := s.claims.Get(r.URL.Query().Get("token"), apiKey)
	if err != nil {
		// Demote the two known-noisy classes to Debug so dashboards aren't
		// dominated by stale-embed traffic (cosmic-crab.buzz and similar
		// hot-link sites with cached/truncated tokens). They generate ~5%
		// of all thp log lines and are not actionable from our side. The
		// 403 still goes back to the client; we just stop shouting about
		// it.
		errMsg := err.Error()
		if strings.Contains(errMsg, "Token is expired") ||
			strings.Contains(errMsg, "invalid number of segments") {
			logger.WithError(err).Debug("failed to get claims (expired/malformed)")
		} else {
			logger.WithError(err).Warn("failed to get claims")
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// If the token carries a `hash` claim, it's bound to that specific torrent
	// and any mismatch is treated as forgery (prevents replay across content).
	if boundHash, _ := claims["hash"].(string); boundHash != "" && boundHash != src.InfoHash {
		logger.WithFields(logrus.Fields{
			"infohash":   src.InfoHash,
			"bound_hash": boundHash,
		}).Warn("token hash mismatch")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	source := Internal
	if r.Header.Get("X-FORWARDED-FOR") != "" {
		source = External
	}
	src.Internal = source == Internal

	ads := false

	role := "nobody"
	if r, ok := claims["role"].(string); ok {
		role = r
	}
	if r, ok := claims["ads"].(bool); ok {
		ads = r
	}
	domain := tokenDomain(claims)

	sessionID := ""
	if sid, ok := claims["sessionID"].(string); ok {
		sessionID = sid
	}

	// Set by ingress-nginx and logged there as $req_id: joins this request
	// to what the client actually received behind the ingress buffer.
	reqID := r.Header.Get("X-Request-ID")

	if s.enforceSessionIP && source == External && sessionID != "" {
		if bound, ok := claims["remoteAddress"].(string); ok && bound != "" {
			reqIP := s.getIP(r)
			if !sameSubnet(bound, reqIP) {
				logger.WithFields(logrus.Fields{
					"session_id": sessionID,
					"session_ip": bound,
					"request_ip": reqIP,
					"infohash":   src.InfoHash,
					"path":       src.Path,
				}).Warn("session IP mismatch")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
	}

	if s.sl != nil && s.sl.Enabled() && source == External {
		release, reason := s.sl.Acquire(sessionID, src.InfoHash, s.sl.limiterPath(src), s.getIP(r))
		if release == nil {
			logger.WithFields(logrus.Fields{
				"session_id": sessionID,
				"infohash":   src.InfoHash,
				"path":       src.Path,
				"request_ip": s.getIP(r),
				"reason":     reason,
			}).Warn("session limiter rejected")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		defer release()
	}

	wi.resolveSize = func(statusCode int) prometheus.Counter {
		return promHTTPProxyRequestSize.WithLabelValues(
			domain,
			role,
			string(source),
			src.GetEdgeName(),
			strconv.Itoa(statusCode/100*100),
		)
	}

	// Set once the bandwidth limiter wraps the writer below; nil means the
	// response was not throttled at all (internal caller, no rate in the
	// token, limiting off), which is not the same as throttled for 0 s.
	var tw *ThrottledResponseWriter

	promHTTPProxyRequestCurrent.WithLabelValues(string(source), role, src.GetEdgeName()).Inc()
	defer func() {
		if outcome.clientGone {
			// errorHandler wrote the 502 it always did (a half-closed
			// client still reads it); from here on, what thp records.
			wi.statusCode = StatusClientClosedRequest
		}
		duration := time.Since(wi.start)
		blocked := wi.Blocked()
		if s.clickHouse != nil && wi.bytesWritten > 0 && wi.GroupedStatusCode() == 200 {
			err := s.clickHouse.Add(&StatRecord{
				ApiKey:        apiKey,
				BytesWritten:  uint64(wi.bytesWritten),
				Domain:        domain,
				Duration:      uint64(duration.Milliseconds()),
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
		promHTTPProxyRequestDuration.WithLabelValues(string(source), role, src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(duration.Seconds())
		if wi.bytesWritten > 0 {
			promHTTPProxyRequestTTFB.WithLabelValues(string(source), role, src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Observe(wi.ttfb.Seconds())
		}
		promHTTPProxyRequestCurrent.WithLabelValues(string(source), role, src.GetEdgeName()).Dec()
		promHTTPProxyRequestTotal.WithLabelValues(string(source), role, src.GetEdgeName(), strconv.Itoa(wi.GroupedStatusCode())).Inc()
		rate, _ := claims["rate"].(string)
		fields := logrus.Fields{
			"domain":     domain,
			"role":       role,
			"source":     string(source),
			"edge":       src.GetEdgeName(),
			"infohash":   src.InfoHash,
			"path":       src.Path,
			"ttfb":       wi.ttfb.Seconds(),
			"duration":   duration.Seconds(),
			"status":     strconv.Itoa(wi.statusCode),
			"rate":       rate,
			"session_id": sessionID,
			"referer":    r.Referer(),
			"bytes":      wi.bytesWritten,
			// Time blocked handing bytes downstream; what is left of
			// duration after it and throttled is upstream and TTFB.
			"downstream_blocked": blocked.Seconds(),
		}
		if reqID != "" {
			fields["req_id"] = reqID
		}
		if tw != nil {
			waited := tw.Waited()
			fields["throttled"] = waited.Seconds()
			// Status streams (the seeder's ?stats=true, warmup) hold a
			// limited connection for minutes and barely write: not content
			// throughput, so they are logged but kept out of every metric.
			if !isEventStream(wi.Header()) {
				edge := src.GetEdgeName()
				promHTTPProxyThrottleWait.WithLabelValues(string(source), role, edge).Add(waited.Seconds())
				promHTTPProxyThrottledDuration.WithLabelValues(string(source), role, edge).Add(duration.Seconds())
				promHTTPProxyThrottledDownstream.WithLabelValues(string(source), role, edge).Add(blocked.Seconds())
				if wi.GroupedStatusCode() == 200 && wi.bytesWritten >= throttleRatioMinSize(rate) {
					// The seeder serves an attachment whenever the key is
					// present; rest-api spells it download=true.
					_, download := r.URL.Query()["download"]
					promHTTPProxyThrottleRatio.WithLabelValues(role, edge, strconv.FormatBool(download)).Observe(throttleRatio(waited, duration))
				}
			}
		}
		l := logger.WithFields(fields)
		if outcome.err != nil {
			l = l.WithError(outcome.err)
		}
		switch {
		case wi.statusCode == StatusClientClosedRequest:
			// The client left before the upstream answered: not a failure
			// to serve, so not Warn. A long duration is a client that gave
			// up waiting for headers: 2,036 of the seeder's 4,131 external
			// ones on 2026-09-25 ended at 30 s or 60 s, client timeouts on
			// a seeder that had not answered yet.
			l.Info("client closed request")
		case wi.GroupedStatusCode() == 500:
			l.Error("failed to serve request")
		case wi.GroupedStatusCode() == 200:
			l.Info("request served successfully")
		default:
			l.Warn("bad request")
		}
	}()

	headers := map[string]string{
		"X-Source-Url":  s.baseURL + "/" + src.InfoHash + escapePathSegments(src.Path) + "?" + src.Query,
		"X-Proxy-Url":   s.baseURL,
		"X-Info-Hash":   src.InfoHash,
		"X-Path":        src.Path,
		"X-Origin-Path": src.OriginPath,
		"X-Full-Path":   "/" + src.InfoHash + "/" + url.PathEscape(strings.TrimPrefix(src.Path, "/")),
		"X-Token":       src.Token,
		"X-Api-Key":     apiKey,
		"X-Session-ID":  sessionID,
	}

	// Set unconditionally, like every other X-* header above: the mod
	// segment is stripped from the path the service sees, so its type and
	// argument travel as headers (e.g. ~tr:pt → "tr", "pt"). A source can be
	// mod-less even when its first path segment happens to equal a mod name
	// (GET /tr/<hash>/... is a valid mod-less source), so a client-supplied
	// X-Mod-Type/X-Mod-Extra must never survive unmodified: Set with an
	// empty string overwrites whatever the client sent.
	setModHeaders(headers, src)

	rate, ok := claims["rate"].(string)
	if ok {
		headers["X-Download-Rate"] = rate
	}

	if s.bandwidthLimit && source == External {
		b, err := s.bucket.Get(claims)
		if err != nil {
			logger.WithError(err).Errorf("failed to get bucket")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if b != nil {
			tw = NewThrottledRequestWrtier(w, b)
			w = tw
		}
	}

	for k, v := range headers {
		r.Header.Set(k, v)
	}

	pr, err := s.pr.Get(src, claims, logger)

	if err != nil {
		logger.WithError(err).Errorf("failed to get proxy")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if pr == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	if s.pr.maxRetries > 0 {
		r = WithRetryContext(r, &RetryContext{
			Src:               src,
			Claims:            claims,
			Logger:            logger,
			SvcLoc:            s.pr.r.svcLoc,
			Cfg:               s.pr.r.cfg,
			Transport:         s.pr.transport,
			ExternalTransport: s.pr.externalTransport,
			MaxRetries:        s.pr.maxRetries,
			RetryDelay:        s.pr.retryDelay,
		})
	}
	r = WithRulesContext(r, &RulesContext{
		Claims:       claims,
		PrimaryToken: r.URL.Query().Get("token"),
		InfoHash:     src.InfoHash,
	})
	r = WithFileKey(r, src.InfoHash, src.Path)
	r = withProxyOutcome(r, outcome)
	// A session's own content feeds GET /session-stats. Internal requests
	// are services fetching on a viewer's behalf (web-ui's own fetches come
	// through the public ingress in prod and do count); grace segment
	// tokens carry no session yet. The stream answers for 40-hex
	// infohashes, and checkHash lets any first segment with 5 hex digits in
	// it through: a key under anything else would be kept and never read.
	if source == External && sessionID != "" && isInfoHash(src.InfoHash) {
		sw := &sessionStatsWriter{
			ResponseWriter: w,
			stats:          s.stats,
			key:            sessionStatsKey{sessionID: sessionID, domain: domain, infoHash: strings.ToLower(src.InfoHash)},
		}
		if isGraceToken(claims) {
			// Its bytes and its time open are the viewer's; its rate and its
			// limiter's wait are the grace window's, not the tier's.
			sw.grace = true
		} else {
			sw.rate, sw.tw = rate, tw
		}
		defer sw.done()
		w = sw
	}
	// The seeder's ?stats event stream (web-ui's status page) never ends on
	// its own: left to the drain, one open status page holds every rollout
	// for the whole shutdown timeout and is cut at its end all the same. It
	// ends as Close begins, with the session-stats streams, and web-ui
	// reopens it on the new pod. The key, not the Content-Type: ?warmup is an
	// event stream too, and its end reads as "warmup complete".
	if _, ok := r.URL.Query()["stats"]; ok {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-s.closing:
				cancel()
			case <-ctx.Done():
			}
		}()
		r = r.WithContext(ctx)
	}
	pr.ServeHTTP(w, r)
}

// Listen binds the web port. run() calls it before any servable starts: the
// probe answers Ready as soon as it listens, and the DaemonSet's maxSurge 1
// retires the old pod on that, so the port must be bound first. Connections
// made before Serve wait in the backlog.
func (s *Web) Listen() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "failed to web listen to tcp connection")
	}
	s.ln = ln
	return nil
}

// Serve serves on the port Listen bound, binding it first if Listen was not
// called. After Close, or once Close has drained it, it returns nil.
func (s *Web) Serve() error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	logrus.Infof("serving Web at %v", s.ln.Addr())
	return s.gs.Serve(&http.Server{
		Handler:        s.newMux(),
		MaxHeaderBytes: 50 << 20,
	}, s.ln)
}

func (s *Web) newMux() *http.ServeMux {
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

	mux.HandleFunc("/speedtest", s.handleSpeedtest)

	mux.HandleFunc(sessionStatsPath, s.handleSessionStats)

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
			// The URL is the client's input: a bad hash, an empty path or an
			// invalid mod argument is their error, not ours.
			logger.WithError(err).Warn("failed to parse url")
			w.WriteHeader(http.StatusBadRequest)
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
	return mux
}

// Close shuts the web server down and returns once in-flight requests are
// done, so run() calls it before closing what their deferred work uses
// (ClickHouse). Streams that never end on their own end first (session-stats
// and the seeder's ?stats, see proxyHTTP): each would hold the drain for the
// whole timeout. Then the listener closes and in-flight requests (downloads)
// get up to the shutdown timeout, after which what is left is cut. The stats
// janitor stops last. Safe to call more than once, concurrently, and before
// Serve; every call waits for the drain.
func (s *Web) Close() {
	s.closeOnce.Do(func() { close(s.closing) })
	s.gs.Close()
	s.stats.Close()
}
