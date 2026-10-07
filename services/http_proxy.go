package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	proxyReadBufferSizeFlag  = "proxy-read-buffer-size"
	proxyWriteBufferSizeFlag = "proxy-write-buffer-size"
	retryMaxAttemptsFlag     = "retry-max-attempts"
	retryDelayFlag           = "retry-delay"
)

type HTTPProxy struct {
	*lazymap.LazyMap[*httputil.ReverseProxy]
	r                 *Resolver
	transport         *http.Transport
	externalTransport *http.Transport
	maxRetries        int
	retryDelay        time.Duration
	fileSizeCache     *FileSizeCache
}

func NewHTTPProxy(c *cli.Context, r *Resolver, retryDelay time.Duration, fsc *FileSizeCache) *HTTPProxy {
	readBuf := c.Int(proxyReadBufferSizeFlag)
	writeBuf := c.Int(proxyWriteBufferSizeFlag)
	ut := c.Duration(upstreamTCPUserTimeoutFlag)
	if ut > 0 && !tcpUserTimeoutSupported {
		logrus.Warnf("TCP_USER_TIMEOUT is Linux-only, %s=%v ignored on this OS", upstreamTCPUserTimeoutFlag, ut)
	}
	dial := upstreamDialer(c.Duration(upstreamKeepAliveFlag), ut)
	p := &HTTPProxy{
		r: r,
		transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
			WriteBufferSize:     writeBuf,
			ReadBufferSize:      readBuf,
		},
		maxRetries:    c.Int(retryMaxAttemptsFlag),
		retryDelay:    retryDelay,
		fileSizeCache: fsc,
		LazyMap: lazymap.New[*httputil.ReverseProxy](&lazymap.Config{
			Expire: 60 * time.Second,
		}),
	}
	p.externalTransport = &http.Transport{
		DialContext: dial,
		// net/http turns HTTP/2 on by itself only for a Transport without a
		// custom dialer; this keeps it for TLS redirect targets as before.
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		WriteBufferSize:     writeBuf,
		ReadBufferSize:      readBuf,
	}
	return p
}

const (
	upstreamKeepAliveFlag      = "upstream-keepalive"
	upstreamTCPUserTimeoutFlag = "upstream-tcp-user-timeout"
	// upstreamKeepAliveCount unanswered probes, keepalive apart, end a
	// connection: with 5 s, 20 s after the last thing received, the same
	// bound TCP_USER_TIMEOUT sets on Linux, where it takes the count's place.
	upstreamKeepAliveCount = 3
)

// upstreamDialer dials thp's upstreams, pods and redirect targets alike, so
// that one which vanished without a FIN or RST is noticed in ~20 s instead
// of 150.
//
// A pod deleted mid-stream can lose its network before its kernel has sent
// the FIN, while unsent data is still queued: thp then receives nothing at
// all, and the read waits for keepalive to give up, which with Go's default
// (15 s idle, 9 probes 15 s apart) takes 150 s. On 2026-10-07 the streams
// the seeder's rollout cut (353) and s3-cache's (23) failed with "read:
// connection timed out" 150-200 s after their pod went away.
//
//   - keepAlive is the idle time and the probe interval. The peer's kernel
//     answers a probe, not its process, so an upstream that sends nothing
//     for minutes (a seeder waiting on a slow swarm, its stall guard up to
//     10 min) stays connected; only a peer whose kernel is gone goes
//     unanswered. Neither does thp's own closed receive window (a slow
//     client, the limiter) stop the answer: it carries no data.
//   - userTimeout sets TCP_USER_TIMEOUT (Linux): the kernel drops the
//     connection once data thp sent stays unacknowledged that long. Keepalive
//     does not probe while data is in flight, so without it a request
//     written into a pooled connection to a pod that has just gone waits for
//     tcp_retries2 (~15 min). With it set, Linux drops an idle connection
//     once nothing has been received for userTimeout and a probe went
//     unanswered, in place of the probe count.
//
// Both 0: Go's default dialer, as before.
func upstreamDialer(keepAlive, userTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{}
	if keepAlive > 0 {
		d.KeepAliveConfig = net.KeepAliveConfig{
			Enable:   true,
			Idle:     keepAlive,
			Interval: keepAlive,
			Count:    upstreamKeepAliveCount,
		}
	}
	if userTimeout > 0 {
		d.Control = func(_, _ string, rc syscall.RawConn) error {
			return setTCPUserTimeout(rc, userTimeout)
		}
	}
	return d.DialContext
}

func RegisterHTTPProxyFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   proxyReadBufferSizeFlag,
			Usage:  "proxy transport read buffer size in bytes",
			Value:  512 << 10,
			EnvVar: "PROXY_READ_BUFFER_SIZE",
		},
		cli.IntFlag{
			Name:   proxyWriteBufferSizeFlag,
			Usage:  "proxy transport write buffer size in bytes",
			Value:  512 << 10,
			EnvVar: "PROXY_WRITE_BUFFER_SIZE",
		},
		cli.IntFlag{
			Name:   retryMaxAttemptsFlag,
			Usage:  "max retry attempts on upstream failure",
			Value:  3,
			EnvVar: "RETRY_MAX_ATTEMPTS",
		},
		cli.IntFlag{
			Name:   retryDelayFlag,
			Usage:  "delay between retry attempts in milliseconds",
			Value:  1000,
			EnvVar: "RETRY_DELAY_MS",
		},
		cli.DurationFlag{
			Name:   upstreamKeepAliveFlag,
			Usage:  "upstream TCP keepalive idle time and probe interval, 3 probes (0 = Go's default, 15s and 9)",
			Value:  5 * time.Second,
			EnvVar: "UPSTREAM_KEEPALIVE",
		},
		cli.DurationFlag{
			Name:   upstreamTCPUserTimeoutFlag,
			Usage:  "drop an upstream connection whose sent data or keepalive probe stays unacknowledged this long (TCP_USER_TIMEOUT, Linux; 0 = kernel default)",
			Value:  20 * time.Second,
			EnvVar: "UPSTREAM_TCP_USER_TIMEOUT",
		},
	)
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

// StatusClientClosedRequest is nginx's 499: the client closed its connection
// before the response began. thp records it (metrics, closing log line) so
// that a client that left is not counted as an upstream failure; it never
// writes it. "Its context is done" is not "nobody reads": net/http cancels
// the context on EOF from the client, and a client that only half-closed
// (FIN on its write side) still reads the answer, so the wire keeps the 502
// it always got.
const StatusClientClosedRequest = 499

type proxyOutcomeKey struct{}

// proxyOutcome ties a proxied request to how the ReverseProxy gave up on it.
// proxyHTTP attaches it to the request and logs err in its closing line.
type proxyOutcome struct {
	// client is the context the server handed proxyHTTP: done once the
	// client's connection is gone. Not the upstream request's context,
	// which proxyHTTP may derive and cancel on its own while the client is
	// still connected (the seeder's ?stats streams on Close).
	client context.Context
	// err is what errorHandler was called with; nil when it was not.
	err error
	// clientGone: err was the client leaving (see clientGone). The wire got
	// 502; proxyHTTP records 499.
	clientGone bool
}

func withProxyOutcome(r *http.Request, o *proxyOutcome) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), proxyOutcomeKey{}, o))
}

// clientGone reports whether the upstream request failed because the client
// left: the transport answers a canceled request with context.Canceled
// (Transport.RoundTrip returns context.Cause, and a canceled dial too), and
// the client's own context is done. The second half keeps thp's own
// cancellations with the client still there 502, and an upstream error that
// came first stays that error even if the client leaves before we answer.
// Measured 2026-09-25 (4 h, all pods): 7,860 of 8,802 thp 502s were
// "context canceled", every one thp produced for the seeder, the transcoder
// and the archiver among them.
func clientGone(client context.Context, err error) bool {
	return client.Err() != nil && errors.Is(err, context.Canceled)
}

// errorHandler replaces ReverseProxy's default, which answers 502 whatever
// the cause, a client that left included, and logs "http: proxy error"
// through the standard logger with nothing to tell which edge it was.
// It writes the same 502 whatever the cause, so nothing on the wire changes;
// it only notes the error and whether it was the client leaving, which
// proxyHTTP records as 499 and logs in its closing line.
func errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if o, ok := r.Context().Value(proxyOutcomeKey{}).(*proxyOutcome); ok {
		o.err = err
		o.clientGone = clientGone(o.client, err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

func (s *HTTPProxy) modifyResponse(r *http.Response) error {
	delCORSHeaders(r.Header)
	s.captureFileSize(r)
	return applyResponseRules(r)
}

// captureFileSize extracts the underlying upstream file size from response
// headers and stores it in fileSizeCache by (infoHash, path) so that
// SessionLimiter can classify the path as big/light on subsequent
// requests. Best-effort and free-running — silently skips when the cache
// isn't wired, the response lacks a usable size header, or the request
// context doesn't carry the file key.
func (s *HTTPProxy) captureFileSize(r *http.Response) {
	if s.fileSizeCache == nil || r.Request == nil {
		return
	}
	fk := GetFileKey(r.Request)
	if fk == nil || fk.InfoHash == "" || fk.Path == "" {
		return
	}
	if size := SizeFromHeaders(r.StatusCode, r.ContentLength, r.Header.Get("Content-Range")); size > 0 {
		s.fileSizeCache.Set(fk.InfoHash, fk.Path, size)
	}
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

// redirectFollowingTransport wraps an http.RoundTripper and follows 302/307
// redirects transparently, preserving the Range header across hops.
// This allows httputil.ReverseProxy to proxy the final response (e.g. from S3)
// instead of passing the redirect back to the client.
type redirectFollowingTransport struct {
	http.RoundTripper
	external http.RoundTripper
}

func (t *redirectFollowingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return t.follow(req, resp)
}

// follow takes resp, the first hop's answer to req, along its redirects.
func (t *redirectFollowingTransport) follow(req *http.Request, resp *http.Response) (*http.Response, error) {
	for i := 0; i < 10; i++ {
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusTemporaryRedirect {
			break
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		_ = resp.Body.Close()
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, loc, nil)
		if err != nil {
			return nil, err
		}
		if rng := req.Header.Get("Range"); rng != "" {
			newReq.Header.Set("Range", rng)
		}
		resp, err = t.external.RoundTrip(newReq)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (s *HTTPProxy) get(loc *Location) (*httputil.ReverseProxy, error) {
	u := &url.URL{
		Host:   fmt.Sprintf("%s:%d", loc.IP.String(), loc.HTTP),
		Scheme: "http",
	}
	var t http.RoundTripper
	if loc.Unavailable {
		t = &stubTransport{s.transport}
	} else {
		t = &redirectFollowingTransport{s.transport, s.externalTransport}
		if s.maxRetries > 0 {
			t = &retryTransport{RoundTripper: t}
		}
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = t
	p.ModifyResponse = s.modifyResponse
	p.ErrorHandler = errorHandler
	p.FlushInterval = -1
	// Strip the client's Accept-Encoding for .m3u8 paths. modifyResponse
	// rewrites segment tokens via byte-level substring match, which silently
	// fails on a gzipped body — needle never found, gzipped body passes
	// through unchanged. With the header gone, the Transport asks for gzip on
	// its own (no Range, not HEAD) and decompresses before modifyResponse:
	// net/http undoes only what it asked for. So the upstream hop may still
	// be compressed (nginx-vod gzips playlists), and the rewrite sees plain
	// text either way (TestProxyHTTP_FMP4PlaylistGrace).
	//
	// The client gets the playlist uncompressed: thp does not compress, and
	// the ingress does not either (its gzip/brotli types leave out
	// application/vnd.apple.mpegurl, checked 2026-09-27). Nor are playlists
	// tiny: every reference carries the token. content-transcoder's
	// playlists through thp, 1 h on 2026-09-27: p50 68 KB, p95 684 KB,
	// max 2.0 MB.
	defaultDirector := p.Director
	p.Director = func(req *http.Request) {
		defaultDirector(req)
		if strings.HasSuffix(req.URL.Path, ".m3u8") {
			req.Header.Del("Accept-Encoding")
		}
	}
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
