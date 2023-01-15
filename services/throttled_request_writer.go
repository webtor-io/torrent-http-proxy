package services

import (
	"bufio"
	"net"
	"net/http"

	"github.com/juju/ratelimit"
	"github.com/pkg/errors"
)

type ThrottledResponseWriter struct {
	http.ResponseWriter
	b *ratelimit.Bucket
}

func NewThrottledRequestWrtier(w http.ResponseWriter, b *ratelimit.Bucket) *ThrottledResponseWriter {
	return &ThrottledResponseWriter{
		ResponseWriter: w,
		b:              b,
	}
}

func (w *ThrottledResponseWriter) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *ThrottledResponseWriter) Write(p []byte) (int, error) {
	w.b.Wait(int64(len(p)))
	return w.ResponseWriter.Write(p)
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
