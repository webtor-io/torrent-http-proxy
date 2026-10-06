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

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

var (
	promRetryAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_retry_attempts_total",
		Help: "Total number of upstream retry attempts; same_pod: the attempt went back to the pod that cut the stream (empty for exhausted, which is no attempt)",
	}, []string{"outcome", "same_pod"})
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
// resp.Body with a retryingReadCloser that transparently reconnects if the
// upstream connection breaks mid-transfer: first to the same pod, then to
// another pod on the same node.
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

	// Resume offset must reflect what the upstream actually served, not what
	// the request asked for: a 200 means Range was ignored (or absent) and
	// the stream starts at byte 0; a 206's true start lives in Content-Range
	// (the server may clamp). Suffix ranges ("bytes=-N") have no absolute
	// start without knowing the object size — if the 206 carries no
	// Content-Range to anchor on, retrying could only splice wrong bytes, so
	// leave the response unwrapped and let the client re-request.
	origStart := int64(0)
	if resp.StatusCode == http.StatusPartialContent {
		if s, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); ok {
			origStart = s
		} else if start, _, _, ok := parseRange(req.Header.Get("Range")); ok {
			origStart = start
		} else {
			return resp, nil
		}
	}

	// The host of the attempt that failed last.
	failedHost := req.URL.Host
	// A pod that answered 200/206 is alive: the stream mostly ends because
	// the seeder cut it (its stall guard, its write deadline; 81% unexpected
	// EOF, 18% connection reset on 2026-10-06), and another pod would load
	// the whole torrent a second time on the node. So the first retry goes
	// back to the same pod while it is still a ready endpoint; after that,
	// and when it is gone, to another pod. A pod given up on is left out of
	// this request's later attempts only (tried). The shared ignore list,
	// which turns a pod off for every torrent for 30 s, is for a pod that
	// could not be reached: a dial error.
	samePodTried := false
	var tried []string

	reconnectFn := func(offset int64) (io.ReadCloser, bool, error) {
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
			return nil, false, errors.New("no service config found")
		}

		samePod := !samePodTried && rc.SvcLoc.Serves(cfg, failedIP)
		targetHost := failedHost
		if samePod {
			samePodTried = true
		} else {
			tried = append(tried, failedIP)
			// Resolve fallback target (same-node pod for K8s, same host for env).
			loc, err := rc.SvcLoc.GetFallback(cfg, rc.Src, tried, rc.Claims)
			if err != nil {
				return nil, false, errors.Wrap(err, "failed to resolve fallback")
			}
			targetHost = fmt.Sprintf("%s:%d", loc.IP, loc.Ports.HTTP)
		}
		failedHost = targetHost

		// Build a new request to the target.
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, fmt.Sprintf("http://%s%s?%s", targetHost, req.URL.Path, req.URL.RawQuery), nil)
		if err != nil {
			return nil, samePod, err
		}
		// Copy relevant headers from original request.
		for _, h := range []string{"X-Source-Url", "X-Proxy-Url", "X-Info-Hash", "X-Path", "X-Origin-Path", "X-Full-Path", "X-Token", "X-Api-Key", "X-Session-ID", "X-Download-Rate", "X-Mod-Type", "X-Mod-Extra", "X-Role"} {
			if v := req.Header.Get(h); v != "" {
				newReq.Header.Set(h, v)
			}
		}
		newReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", newStart))

		// Use the same inner transport chain (redirect-following).
		innerTransport := &redirectFollowingTransport{rc.Transport, rc.ExternalTransport}
		newResp, err := innerTransport.RoundTrip(newReq)
		if err != nil {
			if isDialError(err) {
				ip, _, _ := net.SplitHostPort(targetHost)
				rc.SvcLoc.Ignore(ip)
			}
			return nil, samePod, errors.Wrap(err, "retry request failed")
		}
		if newResp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			// A 416 whose Content-Range total equals our resume offset means
			// the previous connection had already delivered the whole object
			// and just closed dirtily — that's a clean EOF, not a failure.
			totalStr := strings.TrimPrefix(newResp.Header.Get("Content-Range"), "bytes */")
			_ = newResp.Body.Close()
			if total, perr := strconv.ParseInt(totalStr, 10, 64); perr == nil && total == newStart {
				return nil, samePod, errUpstreamEOF
			}
			return nil, samePod, errors.Errorf("expected 206 on retry, got 416 (Content-Range %q, resume offset %d)", newResp.Header.Get("Content-Range"), newStart)
		}
		if newResp.StatusCode != http.StatusPartialContent {
			_ = newResp.Body.Close()
			return nil, samePod, errors.Errorf("expected 206 on retry, got %d", newResp.StatusCode)
		}

		return newResp.Body, samePod, nil
	}

	resp.Body = &retryingReadCloser{
		body:        resp.Body,
		reconnectFn: reconnectFn,
		expected:    resp.ContentLength,
		maxRetries:  rc.MaxRetries,
		retryDelay:  rc.RetryDelay,
		logger: logrus.WithFields(logrus.Fields{
			"component": "retry",
			"infohash":  rc.Src.InfoHash,
			"path":      redactURL(rc.Src.Path),
		}),
	}
	return resp, nil
}

// errUpstreamEOF signals that a reconnect attempt discovered the stream was
// already fully delivered (retry offset == object size) — treat as clean EOF.
var errUpstreamEOF = errors.New("upstream stream already fully delivered")

// retryingReadCloser wraps an io.ReadCloser and transparently reconnects
// on retryable errors, resuming from the byte offset where the error occurred.
type retryingReadCloser struct {
	mu   sync.Mutex
	body io.ReadCloser
	// reconnectFn resumes at offset; samePod: it went back to the pod that
	// cut the stream.
	reconnectFn func(offset int64) (body io.ReadCloser, samePod bool, err error)
	bytesRead   int64
	expected    int64 // Content-Length of the original response, -1 if unknown
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

	// A dirty close after the full body was delivered is a clean EOF, not a
	// failure — retrying from bytesRead would ask for a range past the end
	// and could only ever fail with 416, killing an already-complete stream.
	if r.expected >= 0 && r.bytesRead >= r.expected {
		return 0, io.EOF
	}

	if r.retries >= r.maxRetries {
		r.logger.WithError(err).WithField("outcome", "exhausted").Warnf("upstream failed, retries exhausted (%d/%d)", r.retries, r.maxRetries)
		promRetryAttempts.WithLabelValues("exhausted", "").Inc()
		return 0, err
	}

	r.logger.WithError(err).WithField("bytesRead", r.bytesRead).WithField("retry", r.retries+1).Warn("upstream connection lost, retrying")

	// Close the broken body.
	_ = r.body.Close()

	for {
		// Wait before retry.
		time.Sleep(r.retryDelay)

		// Reconnect.
		newBody, samePod, reconnErr := r.reconnectFn(r.bytesRead)
		r.retries++
		label := strconv.FormatBool(samePod)
		logger := r.logger.WithField("retry", r.retries).WithField("same_pod", samePod)
		if reconnErr == nil {
			r.body = newBody
			logger.WithField("outcome", "success").Info("retry reconnection successful")
			promRetryAttempts.WithLabelValues("success", label).Inc()
			break
		}
		if errors.Is(reconnErr, errUpstreamEOF) {
			logger.WithField("bytesRead", r.bytesRead).WithField("outcome", "eof").Info("retry found stream fully delivered, treating as EOF")
			promRetryAttempts.WithLabelValues("eof", label).Inc()
			return 0, io.EOF
		}
		logger.WithError(reconnErr).WithField("originalError", err.Error()).WithField("outcome", "failure").Warn("retry reconnection failed")
		promRetryAttempts.WithLabelValues("failure", label).Inc()
		// The pod that cut the stream did not take it back: the next
		// attempt, within the same budget, goes to another pod. Any other
		// failed attempt ends the stream, as does the client leaving.
		if !samePod || r.retries >= r.maxRetries || errors.Is(reconnErr, context.Canceled) {
			return 0, err // return original error
		}
	}

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

// isDialError: the attempt got no connection — the pod is gone or not
// listening (refused, unreachable, connect timeout). Not a dial the client
// cut short by leaving: the pod may be fine.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" && !errors.Is(err, context.Canceled)
}

// parseRange parses "bytes=start-end" or "bytes=start-" into start and end
// values. ok is false for anything without an absolute start — notably the
// suffix form "bytes=-N", whose real offset depends on the object size and
// must never be treated as start=0.
func parseRange(rangeHeader string) (start int64, end int64, hasEnd bool, ok bool) {
	rangeHeader = strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeHeader, "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false, false
	}
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false, false
		}
		hasEnd = true
	}
	return start, end, hasEnd, true
}

// parseContentRangeStart extracts the start offset from a
// "bytes <start>-<end>/<total>" Content-Range header.
func parseContentRangeStart(h string) (int64, bool) {
	h = strings.TrimPrefix(h, "bytes ")
	dash := strings.IndexByte(h, '-')
	if dash <= 0 {
		return 0, false
	}
	start, err := strconv.ParseInt(h[:dash], 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}
