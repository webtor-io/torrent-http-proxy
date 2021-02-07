package services

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/pkg/errors"
)

type responseWriterInterceptor struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	start        time.Time
	ttfb         time.Duration
}

func NewResponseWrtierInterceptor(w http.ResponseWriter) *responseWriterInterceptor {
	return &responseWriterInterceptor{
		statusCode:     http.StatusOK,
		ResponseWriter: w,
		start:          time.Now(),
	}
}

func (w *responseWriterInterceptor) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
func (w *responseWriterInterceptor) GroupedStatusCode() int {
	return w.statusCode / 100 * 100
}

func (w *responseWriterInterceptor) Write(p []byte) (int, error) {
	if w.bytesWritten == 0 {
		w.ttfb = time.Since(w.start)
	}
	w.bytesWritten += len(p)
	return w.ResponseWriter.Write(p)
}

func (w *responseWriterInterceptor) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *responseWriterInterceptor) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &responseWriterInterceptor{}
	_ http.Hijacker       = &responseWriterInterceptor{}
	_ http.Flusher        = &responseWriterInterceptor{}
)
