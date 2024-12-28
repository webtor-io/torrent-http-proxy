package services

import (
	"bytes"
	"context"
	"fmt"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sirupsen/logrus"
)

const (
	httpProxyRedialTriesFlag = "http-proxy-redial-tries"
	httpProxyRedialDelayFlag = "http-proxy-redial-delay"
)

func RegisterHTTPProxyFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   httpProxyRedialTriesFlag,
			Usage:  "HTTP proxy redial tries",
			Value:  2,
			EnvVar: "HTTP_PROXY_REDIAL_TRIES",
		},
		cli.IntFlag{
			Name:   httpProxyRedialDelayFlag,
			Usage:  "HTTP proxy redial delay (sec)",
			Value:  1,
			EnvVar: "HTTP_PROXY_REDIAL_DELAY",
		},
	)
}

var (
	promHTTPProxyDialDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "webtor_http_proxy_dial_duration_seconds",
		Help: "HTTP Proxy dial duration in seconds",
	}, []string{"name"})
	promHTTPProxyDialCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_dial_current",
		Help: "HTTP Proxy dial current",
	}, []string{"name"})
	promHTTPProxyDialTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_dial_total",
		Help: "HTTP Proxy dial total",
	}, []string{"name"})
	promHTTPProxyDialErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_dial_errors",
		Help: "HTTP Proxy dial errors",
	}, []string{"name"})
)

func init() {
	prometheus.MustRegister(promHTTPProxyDialDuration)
	prometheus.MustRegister(promHTTPProxyDialCurrent)
	prometheus.MustRegister(promHTTPProxyDialTotal)
	prometheus.MustRegister(promHTTPProxyDialErrors)
}

type HTTPProxy struct {
	lazymap.LazyMap[*httputil.ReverseProxy]
	r     *Resolver
	tries int
	delay int
}

func NewHTTPProxy(c *cli.Context, r *Resolver) *HTTPProxy {
	return &HTTPProxy{
		r:     r,
		tries: c.Int(httpProxyRedialTriesFlag),
		delay: c.Int(httpProxyRedialDelayFlag),
		LazyMap: lazymap.New[*httputil.ReverseProxy](&lazymap.Config{
			Expire:      600 * time.Second,
			StoreErrors: false,
		}),
	}
}

var corsHeaders = []string{
	"Access-Control-Allow-Credentials",
	"Access-Control-Allow-Origin",
}

func delCORSHeaders(header http.Header) {
	for _, h := range corsHeaders {
		header.Del(h)
	}
}

func modifyResponse(r *http.Response) error {
	delCORSHeaders(r.Header)
	return nil
}

func (s *HTTPProxy) dialWithRetry(ctx context.Context, loc *Location, src *Source, network string, tries int, delay int, logger *logrus.Entry) (conn net.Conn, err error) {
	now := time.Now()
	promHTTPProxyDialCurrent.WithLabelValues(src.GetEdgeName()).Inc()
	defer func() {
		promHTTPProxyDialTotal.WithLabelValues(src.GetEdgeName()).Inc()
		promHTTPProxyDialCurrent.WithLabelValues(src.GetEdgeName()).Dec()
		promHTTPProxyDialDuration.WithLabelValues(src.GetEdgeName()).Observe(time.Since(now).Seconds())
	}()
	for i := 0; i < tries; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		conn, err = s.dial(ctx, loc, network, logger)
		if err != nil {
			time.Sleep(time.Duration(delay) * time.Second)
			loc, err = s.r.Resolve(ctx, src, logger, true)
			if err != nil {
				logger.WithError(err).Error("failed to get location")
				return nil, err
			}
		} else {
			break
		}
	}
	if conn == nil {
		logger.WithError(err).Error("failed to dial")
		promHTTPProxyDialErrors.WithLabelValues(src.GetEdgeName()).Inc()
	}
	return
}

func (s *HTTPProxy) dial(ctx context.Context, loc *Location, network string, logger *logrus.Entry) (net.Conn, error) {
	logger.Info("dialing proxy backend")
	addr := fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP)
	conn, err := (&net.Dialer{
		Timeout:   1 * time.Minute,
		KeepAlive: 1 * time.Minute,
	}).DialContext(ctx, network, addr)
	if err != nil {
		logger.WithError(err).Warnf("failed to dial")
		return nil, err
	}
	return conn, nil
}

type stubTransport struct {
	http.RoundTripper
}

func (t *stubTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	return &http.Response{
		Status:        "503 Service Unavailable",
		StatusCode:    503,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          io.NopCloser(bytes.NewBufferString("")),
		ContentLength: int64(0),
		Request:       req,
		Header:        make(http.Header),
	}, nil
}

func (s *HTTPProxy) get(loc *Location, src *Source, logger *logrus.Entry) (*httputil.ReverseProxy, error) {
	u := &url.URL{
		Host:   fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP),
		Scheme: "http",
	}
	var t http.RoundTripper
	if loc.Unavailable {
		t = &stubTransport{http.DefaultTransport}
	} else {
		t = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return s.dialWithRetry(ctx, loc, src, network, s.tries, s.delay, logger)
			},
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			MaxConnsPerHost:     500,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = t
	p.ModifyResponse = modifyResponse
	// p.FlushInterval = -1
	return p, nil
}

func (s *HTTPProxy) Get(ctx context.Context, src *Source, logger *logrus.Entry) (*httputil.ReverseProxy, error) {
	loc, err := s.r.Resolve(ctx, src, logger, false)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get location")
	}
	return s.LazyMap.Get(loc.IP.String(), func() (*httputil.ReverseProxy, error) {
		return s.get(loc, src, logger)
	})
}
