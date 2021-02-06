package services

import (
	"io"
	"net/http"
)

type NoBuffWriter struct {
	w http.ResponseWriter
}

func NewNoBuffWriter(w http.ResponseWriter) *NoBuffWriter {
	return &NoBuffWriter{w: w}
}

func (s *NoBuffWriter) Flush() {
	if w, ok := s.w.(http.Flusher); ok {
		w.Flush()
	}
}

func (s *NoBuffWriter) ReadFrom(r io.Reader) (n int64, err error) {
	if rr, ok := r.(io.WriterTo); ok {
		n, err = rr.WriteTo(s.w)
	} else {
		n, err = io.Copy(s.w, r)
	}
	if n > 0 {
		s.Flush()
	}
	return
}
func (s *NoBuffWriter) Write(p []byte) (n int, err error) {
	n, err = s.w.Write(p)
	if n > 0 {
		s.Flush()
	}
	return
}
func (s *NoBuffWriter) WriteHeader(statusCode int) {
	s.w.WriteHeader(statusCode)
	s.Flush()
}

func (s *NoBuffWriter) Header() http.Header {
	return s.w.Header()
}

func (s *NoBuffWriter) CloseNotify() <-chan bool {
	if w, ok := s.w.(http.CloseNotifier); ok {
		return w.CloseNotify()
	}
	panic("Not implemented")
}
