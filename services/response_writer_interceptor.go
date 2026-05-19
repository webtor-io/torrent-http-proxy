package services

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

type ResponseWriterInterceptor struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	start        time.Time
	ttfb         time.Duration

	// resolveSize is called once on the first Write to obtain a label-bound
	// Counter handle. Lazy because the status label isn't final until the
	// upstream's WriteHeader fires (which precedes the first body Write).
	resolveSize func(statusCode int) prometheus.Counter
	sizeCounter prometheus.Counter
}

func NewResponseWrtierInterceptor(w http.ResponseWriter) *ResponseWriterInterceptor {
	return &ResponseWriterInterceptor{
		statusCode:     http.StatusOK,
		ResponseWriter: w,
		start:          time.Now(),
	}
}

func (w *ResponseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
func (w *ResponseWriterInterceptor) GroupedStatusCode() int {
	return w.statusCode / 100 * 100
}

func (w *ResponseWriterInterceptor) Write(p []byte) (int, error) {
	if w.bytesWritten == 0 {
		w.ttfb = time.Since(w.start)
		if w.resolveSize != nil {
			w.sizeCounter = w.resolveSize(w.statusCode)
		}
	}
	n := len(p)
	w.bytesWritten += n
	if w.sizeCounter != nil {
		w.sizeCounter.Add(float64(n))
	}
	return w.ResponseWriter.Write(p)
}

func (w *ResponseWriterInterceptor) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *ResponseWriterInterceptor) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &ResponseWriterInterceptor{}
	_ http.Hijacker       = &ResponseWriterInterceptor{}
	_ http.Flusher        = &ResponseWriterInterceptor{}
)
