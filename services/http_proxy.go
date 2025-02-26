package services

import (
	"bytes"
	"fmt"
	"github.com/dgrijalva/jwt-go"
	"github.com/webtor-io/lazymap"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type HTTPProxy struct {
	lazymap.LazyMap[*httputil.ReverseProxy]
	r *Resolver
}

func NewHTTPProxy(r *Resolver) *HTTPProxy {
	return &HTTPProxy{
		r: r,
		LazyMap: lazymap.New[*httputil.ReverseProxy](&lazymap.Config{
			Expire: 600 * time.Second,
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

func (s *HTTPProxy) get(loc *Location) (*httputil.ReverseProxy, error) {
	u := &url.URL{
		Host:   fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP),
		Scheme: "http",
	}
	var t http.RoundTripper
	if loc.Unavailable {
		t = &stubTransport{http.DefaultTransport}
	} else {
		t = &http.Transport{
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

func (s *HTTPProxy) Get(src *Source, claims jwt.MapClaims, logger *logrus.Entry) (*httputil.ReverseProxy, error) {
	loc, err := s.r.Resolve(src, claims, logger)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get location")
	}
	return s.LazyMap.Get(fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP), func() (*httputil.ReverseProxy, error) {
		return s.get(loc)
	})
}
