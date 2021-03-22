package services

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sirupsen/logrus"
)

const (
	HTTP_PROXY_DIAL_TIMEOUT int = 3
	HTTP_PROXY_DIAL_TRIES   int = 3
	HTTP_PROXY_REDIAL_DELAY int = 1
	MAX_IDLE_CONNECTIONS    int = 20 * 5
)

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
	proxy  *httputil.ReverseProxy
	logger *logrus.Entry
	inited bool
	mux    sync.Mutex
	err    error
	r      *Resolver
	src    *Source
	invoke bool
	cl     *Client
}

func NewHTTPProxy(r *Resolver, src *Source, logger *logrus.Entry, invoke bool, cl *Client) *HTTPProxy {
	return &HTTPProxy{r: r, src: src, inited: false, logger: logger, invoke: invoke, cl: cl}
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

func (s *HTTPProxy) dialWithRetry(network string, tries int, delay int) (conn net.Conn, err error) {
	now := time.Now()
	promHTTPProxyDialCurrent.WithLabelValues(s.src.GetEdgeName()).Inc()
	defer func() {
		promHTTPProxyDialTotal.WithLabelValues(s.src.GetEdgeName()).Inc()
		promHTTPProxyDialCurrent.WithLabelValues(s.src.GetEdgeName()).Dec()
		promHTTPProxyDialDuration.WithLabelValues(s.src.GetEdgeName()).Observe(time.Since(now).Seconds())
	}()
	purge := false
	for i := 0; i < tries; i++ {
		conn, err = s.dial(network, purge)
		if err != nil {
			purge = true
			time.Sleep(time.Duration(delay) * time.Second)
		} else {
			break
		}
	}
	if err != nil {
		promHTTPProxyDialErrors.WithLabelValues(s.src.GetEdgeName()).Inc()
	}
	return
}

func (s *HTTPProxy) dial(network string, purge bool) (net.Conn, error) {
	s.logger.Info("Dialing proxy backend")
	loc, err := s.r.Resolve(s.src, s.logger, purge, s.invoke, s.cl)
	if err != nil {
		s.logger.WithError(err).Error("Failed to get location")
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP)
	conn, err := (&net.Dialer{
		Timeout:   1 * time.Minute,
		KeepAlive: 1 * time.Minute,
	}).Dial(network, addr)
	if err != nil {
		s.logger.WithError(err).Error("Failed to dial")
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
		Body:          ioutil.NopCloser(bytes.NewBufferString("")),
		ContentLength: int64(0),
		Request:       req,
		Header:        make(http.Header, 0),
	}, nil
}

func (s *HTTPProxy) get() (*httputil.ReverseProxy, error) {
	loc, err := s.r.Resolve(s.src, s.logger, false, s.invoke, s.cl)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get location")
	}
	u := &url.URL{
		Host:   fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP),
		Scheme: "http",
	}

	var t http.RoundTripper
	if loc.Unavailable {
		t = &stubTransport{http.DefaultTransport}
	} else {
		t = &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return s.dialWithRetry(network, HTTP_PROXY_DIAL_TRIES, HTTP_PROXY_REDIAL_DELAY)
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
	p.FlushInterval = -1
	return p, nil
}

func (s *HTTPProxy) Get() (*httputil.ReverseProxy, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.proxy, s.err
	}
	s.proxy, s.err = s.get()
	s.inited = true
	return s.proxy, s.err
}
