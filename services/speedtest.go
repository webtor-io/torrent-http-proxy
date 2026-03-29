package services

import (
	"io"
	"net/http"
	"strconv"

	"github.com/sirupsen/logrus"
)

const (
	speedtestDefaultSize = 10 * 1024 * 1024 // 10MB
	speedtestMinSize     = 1 * 1024 * 1024  // 1MB
	speedtestMaxSize     = 50 * 1024 * 1024  // 50MB
)

// zeroReader implements io.Reader that produces zero bytes.
type zeroReader struct{}

func (z zeroReader) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (s *Web) handleSpeedtest(w http.ResponseWriter, r *http.Request) {
	logger := logrus.WithField("handler", "speedtest")

	// Validate claims
	_, err := s.claims.Get(r.URL.Query().Get("token"), r.URL.Query().Get("api-key"))
	if err != nil {
		logger.WithError(err).Warn("failed to get claims for speedtest")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Parse size parameter
	size := speedtestDefaultSize
	if sizeStr := r.URL.Query().Get("size"); sizeStr != "" {
		parsed, err := strconv.Atoi(sizeStr)
		if err == nil {
			size = parsed
		}
	}
	if size < speedtestMinSize {
		size = speedtestMinSize
	}
	if size > speedtestMaxSize {
		size = speedtestMaxSize
	}

	// Set headers — no rate limiting, raw speed measurement
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	// Stream zero bytes
	written, err := io.Copy(w, io.LimitReader(zeroReader{}, int64(size)))
	if err != nil {
		logger.WithError(err).WithField("written", written).Debug("speedtest stream interrupted")
	}
}
