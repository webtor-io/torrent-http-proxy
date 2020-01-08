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

	"github.com/sirupsen/logrus"
)

const (
	HTTP_PROXY_DIAL_TIMEOUT int = 10
	MAX_IDLE_CONNECTIONS    int = 20 * 5
)

type HTTPProxy struct {
	proxy  *httputil.ReverseProxy
	logger *logrus.Entry
	inited bool
	reloc  func() (*Location, string, error)
	mux    sync.Mutex
	err    error
	r      *Resolver
	src    *Source
	invoke bool
}

func NewHTTPProxy(r *Resolver, src *Source, logger *logrus.Entry, invoke bool) *HTTPProxy {
	return &HTTPProxy{r: r, src: src, inited: false, logger: logger, invoke: invoke}
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

func (s *HTTPProxy) dial(network, addr string) (net.Conn, error) {
	s.logger.Info("Dialing proxy backend")
	timeout := time.Duration(HTTP_PROXY_DIAL_TIMEOUT) * time.Second
	conn, err := (&net.Dialer{
		Timeout: timeout,
	}).Dial(network, addr)
	if err != nil {
		s.logger.Warn("Failed to dial location, try to refresh it")
		loc, err := s.r.Resolve(s.src, s.logger, true, s.invoke)
		if err != nil {
			s.logger.WithError(err).Error("Failed to get new location")
			return nil, err
		}
		addr := fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP)
		conn, err := (&net.Dialer{
			Timeout: timeout,
		}).Dial(network, addr)
		if err != nil {
			s.logger.WithError(err).Error("Failed to dial with new address")
			return nil, err
		}
		return conn, err
	}
	return conn, err
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
	loc, err := s.r.Resolve(s.src, s.logger, false, s.invoke)
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
			Dial:                s.dial,
			MaxIdleConnsPerHost: MAX_IDLE_CONNECTIONS,
		}
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = t
	p.ModifyResponse = modifyResponse
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
