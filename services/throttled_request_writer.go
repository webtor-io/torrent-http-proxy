package services

import (
	"bufio"
	"net"
	"net/http"

	"github.com/juju/ratelimit"
	"github.com/pkg/errors"
)

type throttledResponseWriter struct {
	http.ResponseWriter
	b *ratelimit.Bucket
}

func NewThrottledRequestWrtier(w http.ResponseWriter, b *ratelimit.Bucket) *throttledResponseWriter {
	return &throttledResponseWriter{
		ResponseWriter: w,
		b:              b,
	}
}

func (w *throttledResponseWriter) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *throttledResponseWriter) Write(p []byte) (int, error) {
	w.b.Wait(int64(len(p)))
	return w.ResponseWriter.Write(p)
}

func (w *throttledResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *throttledResponseWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &throttledResponseWriter{}
	_ http.Hijacker       = &throttledResponseWriter{}
	_ http.Flusher        = &throttledResponseWriter{}
)
