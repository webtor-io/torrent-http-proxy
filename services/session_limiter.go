package services

import (
	"sync"
	"sync/atomic"

	"github.com/urfave/cli"
)

const (
	MaxConcPerTorrentFlag = "max-conc-per-torrent"
	MaxConcTotalFlag      = "max-conc-total"
)

func RegisterSessionLimiterFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   MaxConcPerTorrentFlag,
			Usage:  "max concurrent requests per session per torrent (0 = unlimited)",
			Value:  10,
			EnvVar: "MAX_CONC_PER_TORRENT",
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
// (session, infohash) pair. Zero values mean unlimited.
type SessionLimiter struct {
	maxPerTorrent int
	maxTotal      int

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	total    atomic.Int32
	mu       sync.Mutex
	torrents map[string]*atomic.Int32
}

func NewSessionLimiter(c *cli.Context) *SessionLimiter {
	return &SessionLimiter{
		maxPerTorrent: c.Int(MaxConcPerTorrentFlag),
		maxTotal:      c.Int(MaxConcTotalFlag),
		sessions:      make(map[string]*sessionState),
	}
}

func (l *SessionLimiter) Enabled() bool {
	return l.maxPerTorrent > 0 || l.maxTotal > 0
}

func (l *SessionLimiter) getSession(sessionID string) *sessionState {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[sessionID]
	if !ok {
		s = &sessionState{torrents: make(map[string]*atomic.Int32)}
		l.sessions[sessionID] = s
	}
	return s
}

func (s *sessionState) getTorrentCounter(hash string) *atomic.Int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.torrents[hash]
	if !ok {
		c = &atomic.Int32{}
		s.torrents[hash] = c
	}
	return c
}

// Acquire tries to acquire a slot. Returns a release function on success, or nil if rejected.
func (l *SessionLimiter) Acquire(sessionID string, infoHash string) (release func()) {
	if sessionID == "" {
		return func() {}
	}

	s := l.getSession(sessionID)

	// Check total limit first.
	if l.maxTotal > 0 && int(s.total.Load()) >= l.maxTotal {
		return nil
	}

	// Check per-torrent limit.
	tc := s.getTorrentCounter(infoHash)
	if l.maxPerTorrent > 0 && int(tc.Load()) >= l.maxPerTorrent {
		return nil
	}

	s.total.Add(1)
	tc.Add(1)

	return func() {
		tc.Add(-1)
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
