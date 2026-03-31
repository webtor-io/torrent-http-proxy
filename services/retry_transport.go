package services

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"context"

	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

var (
	promRetryAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_retry_attempts_total",
		Help: "Total number of upstream retry attempts",
	}, []string{"outcome"})
)

func init() {
	prometheus.MustRegister(promRetryAttempts)
}

type retryContextKey struct{}

// RetryContext carries per-request data needed to reconnect to an alternative pod.
type RetryContext struct {
	Src               *Source
	Claims            jwt.MapClaims
	Logger            *logrus.Entry
	SvcLoc            *ServiceLocation
	Cfg               *ServicesConfig
	Transport         *http.Transport
	ExternalTransport *http.Transport
	MaxRetries        int
	RetryDelay        time.Duration
}

// WithRetryContext injects RetryContext into the request context.
func WithRetryContext(r *http.Request, rc *RetryContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), retryContextKey{}, rc))
}

// retryTransport wraps a RoundTripper. On successful 200/206 responses it replaces
// resp.Body with a retryingReadCloser that transparently reconnects to another pod
// on the same node if the upstream connection breaks mid-transfer.
type retryTransport struct {
	http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	rc, ok := req.Context().Value(retryContextKey{}).(*RetryContext)
	if !ok || rc == nil || resp.Body == nil || rc.MaxRetries <= 0 {
		return resp, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return resp, nil
	}

	// Parse the original request's Range header to know the starting offset.
	origStart := int64(0)
	if rng := req.Header.Get("Range"); rng != "" {
		origStart, _, _ = parseRange(rng)
	}

	// Capture the failed pod's IP from the request host.
	failedHost := req.URL.Host

	reconnectFn := func(offset int64) (io.ReadCloser, error) {
		newStart := origStart + offset

		// Extract the failed IP (strip port).
		failedIP, _, _ := net.SplitHostPort(failedHost)
		if failedIP == "" {
			failedIP = failedHost
		}

		// Resolve service config for this edge type.
		edgeType := rc.Src.GetEdgeType()
		role, _ := rc.Claims["role"].(string)
		cfg := rc.Cfg.GetMod(fmt.Sprintf("%s-%s", edgeType, role))
		if cfg == nil {
			cfg = rc.Cfg.GetMod(edgeType)
		}
		if cfg == nil {
			return nil, errors.New("no service config found")
		}

		// Resolve fallback target (same-node pod for K8s, same host for env).
		loc, err := rc.SvcLoc.GetFallback(cfg, rc.Src, net.ParseIP(failedIP), rc.Claims)
		if err != nil {
			return nil, errors.Wrap(err, "failed to resolve fallback")
		}
		targetHost := fmt.Sprintf("%s:%d", loc.IP, loc.Ports.HTTP)

		// Build a new request to the target.
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, fmt.Sprintf("http://%s%s?%s", targetHost, req.URL.Path, req.URL.RawQuery), nil)
		if err != nil {
			return nil, err
		}
		// Copy relevant headers from original request.
		for _, h := range []string{"X-Source-Url", "X-Proxy-Url", "X-Info-Hash", "X-Path", "X-Origin-Path", "X-Full-Path", "X-Token", "X-Api-Key", "X-Session-ID", "X-Download-Rate"} {
			if v := req.Header.Get(h); v != "" {
				newReq.Header.Set(h, v)
			}
		}
		newReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", newStart))

		// Use the same inner transport chain (redirect-following).
		innerTransport := &redirectFollowingTransport{rc.Transport, rc.ExternalTransport}
		newResp, err := innerTransport.RoundTrip(newReq)
		if err != nil {
			return nil, errors.Wrap(err, "retry request failed")
		}
		if newResp.StatusCode != http.StatusPartialContent {
			_ = newResp.Body.Close()
			return nil, errors.Errorf("expected 206 on retry, got %d", newResp.StatusCode)
		}

		// Update failedHost for potential subsequent retries.
		failedHost = targetHost

		return newResp.Body, nil
	}

	resp.Body = &retryingReadCloser{
		body:        resp.Body,
		reconnectFn: reconnectFn,
		maxRetries:  rc.MaxRetries,
		retryDelay:  rc.RetryDelay,
		logger: logrus.WithFields(logrus.Fields{
			"component": "retry",
			"infohash":  rc.Src.InfoHash,
			"path":      rc.Src.Path,
		}),
	}
	return resp, nil
}

// retryingReadCloser wraps an io.ReadCloser and transparently reconnects
// on retryable errors, resuming from the byte offset where the error occurred.
type retryingReadCloser struct {
	mu          sync.Mutex
	body        io.ReadCloser
	reconnectFn func(offset int64) (io.ReadCloser, error)
	bytesRead   int64
	maxRetries  int
	retryDelay  time.Duration
	retries     int
	logger      *logrus.Entry
	closed      bool
}

func (r *retryingReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, io.ErrClosedPipe
	}

	n, err := r.body.Read(p)
	if n > 0 {
		r.bytesRead += int64(n)
	}
	if err == nil || err == io.EOF {
		return n, err
	}

	// Return partial data if we got some, retry on the next call.
	if n > 0 {
		return n, nil
	}

	// Check if error is retryable.
	if !isRetryableError(err) {
		return 0, err
	}

	if r.retries >= r.maxRetries {
		r.logger.WithError(err).Warnf("upstream failed, retries exhausted (%d/%d)", r.retries, r.maxRetries)
		promRetryAttempts.WithLabelValues("exhausted").Inc()
		return 0, err
	}

	r.logger.WithError(err).WithField("bytesRead", r.bytesRead).WithField("retry", r.retries+1).Warn("upstream connection lost, retrying on another pod")

	// Close the broken body.
	_ = r.body.Close()

	// Wait before retry.
	time.Sleep(r.retryDelay)

	// Reconnect.
	newBody, reconnErr := r.reconnectFn(r.bytesRead)
	r.retries++
	if reconnErr != nil {
		r.logger.WithError(reconnErr).WithField("originalError", err.Error()).Warn("retry reconnection failed")
		promRetryAttempts.WithLabelValues("failure").Inc()
		return 0, err // return original error
	}

	r.body = newBody
	r.logger.WithField("retry", r.retries).Info("retry reconnection successful")
	promRetryAttempts.WithLabelValues("success").Inc()

	// Read from the new body.
	n, err = r.body.Read(p)
	if n > 0 {
		r.bytesRead += int64(n)
	}
	return n, err
}

func (r *retryingReadCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// isRetryableError returns true for connection-level errors that indicate the
// upstream pod died, not application-level errors or normal completion.
func isRetryableError(err error) bool {
	if err == nil || err == io.EOF {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if err == io.ErrUnexpectedEOF {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	// Check for "connection reset by peer" in error string as fallback.
	msg := err.Error()
	if strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "unexpected EOF") {
		return true
	}
	return false
}

// parseRange parses "bytes=start-end" or "bytes=start-" into start and end values.
func parseRange(rangeHeader string) (start int64, end int64, hasEnd bool) {
	rangeHeader = strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeHeader, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, _ = strconv.ParseInt(parts[0], 10, 64)
	if parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
		hasEnd = true
	}
	return
}
