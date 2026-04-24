package services

import (
	"sync"
	"sync/atomic"

	"github.com/urfave/cli"
)

const (
	MaxConcPerPathFlag = "max-conc-per-path"
	MaxConcTotalFlag   = "max-conc-total"
)

func RegisterSessionLimiterFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   MaxConcPerPathFlag,
			Usage:  "max concurrent requests per session per (torrent, path) (0 = unlimited)",
			Value:  10,
			EnvVar: "MAX_CONC_PER_PATH",
		},
		cli.IntFlag{
			Name:   MaxConcTotalFlag,
			Usage:  "max concurrent requests per session total (0 = unlimited)",
			Value:  30,
			EnvVar: "MAX_CONC_TOTAL",
		},
	)
}

// SessionLimiter limits concurrent requests per session and per
// (session, infohash, path) triple. Zero values mean unlimited.
// Scoping by path (not just infohash) lets HLS playback spread its
// segment requests across many counters while a download accelerator
// hammering a single file with parallel range requests hits one.
type SessionLimiter struct {
	maxPerPath int
	maxTotal   int

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	total atomic.Int32
	mu    sync.Mutex
	paths map[string]*atomic.Int32
}

func NewSessionLimiter(c *cli.Context) *SessionLimiter {
	return &SessionLimiter{
		maxPerPath: c.Int(MaxConcPerPathFlag),
		maxTotal:   c.Int(MaxConcTotalFlag),
		sessions:   make(map[string]*sessionState),
	}
}

func (l *SessionLimiter) Enabled() bool {
	return l.maxPerPath > 0 || l.maxTotal > 0
}

func (l *SessionLimiter) getSession(sessionID string) *sessionState {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[sessionID]
	if !ok {
		s = &sessionState{paths: make(map[string]*atomic.Int32)}
		l.sessions[sessionID] = s
	}
	return s
}

func (s *sessionState) getPathCounter(key string) *atomic.Int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.paths[key]
	if !ok {
		c = &atomic.Int32{}
		s.paths[key] = c
	}
	return c
}

// Acquire tries to acquire a slot. Returns a release function on success, or nil if rejected.
func (l *SessionLimiter) Acquire(sessionID string, infoHash string, path string) (release func()) {
	if sessionID == "" {
		return func() {}
	}

	s := l.getSession(sessionID)

	// Check total limit first.
	if l.maxTotal > 0 && int(s.total.Load()) >= l.maxTotal {
		return nil
	}

	// Check per-(torrent, path) limit.
	pathKey := infoHash + "|" + path
	pc := s.getPathCounter(pathKey)
	if l.maxPerPath > 0 && int(pc.Load()) >= l.maxPerPath {
		return nil
	}

	s.total.Add(1)
	pc.Add(1)

	return func() {
		pc.Add(-1)
		newTotal := s.total.Add(-1)
		if newTotal <= 0 {
			l.mu.Lock()
			if s.total.Load() <= 0 {
				delete(l.sessions, sessionID)
			}
			l.mu.Unlock()
		}
	}
}
