package services

import (
	"bufio"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

type ThrottledResponseWriter struct {
	http.ResponseWriter
	b Throttler
	// waited is the wall time Write has spent blocked in b.Wait, in
	// nanoseconds. Atomic so Waited is safe from any goroutine. Today both
	// readers are on the handler goroutine: sessionStatsWriter.Write after
	// every Write, and proxyHTTP's deferred closure once ServeHTTP returns.
	// ReverseProxy's flush timer calls Flush only, which never touches it.
	waited atomic.Int64
}

func NewThrottledRequestWrtier(w http.ResponseWriter, b Throttler) *ThrottledResponseWriter {
	return &ThrottledResponseWriter{
		ResponseWriter: w,
		b:              b,
	}
}

func (w *ThrottledResponseWriter) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *ThrottledResponseWriter) Write(p []byte) (int, error) {
	start := time.Now()
	w.b.Wait(int64(len(p)))
	w.waited.Add(int64(time.Since(start)))
	return w.ResponseWriter.Write(p)
}

// Waited reports how long the response has been held back by the limiter.
// Time the downstream Write blocks is not counted here; the interceptor
// underneath measures it (ResponseWriterInterceptor.Blocked). Downstream is
// ingress-nginx, not the client: it buffers ~260 MiB per response, so until
// that fills, a client slower than the tier still lets the limiter pace the
// response, and it reads tier-bound. A segment or small range fits that
// buffer whole, so on HLS it never fills: every segment reads tier-bound,
// and a slow client shows only in how seldom it asks for the next one. Only
// the ingress log (joined by req_id) sees what the client took.
func (w *ThrottledResponseWriter) Waited() time.Duration {
	return time.Duration(w.waited.Load())
}

func (w *ThrottledResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *ThrottledResponseWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &ThrottledResponseWriter{}
	_ http.Hijacker       = &ThrottledResponseWriter{}
	_ http.Flusher        = &ThrottledResponseWriter{}
)
