package services

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfave/cli"
)

const (
	MaxConcPerPathFlag   = "max-conc-per-path"
	MaxConcTotalFlag     = "max-conc-total"
	MaxIPsPerSessionFlag = "max-ips-per-session"
)

const ipWindow = 60 * time.Second

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
		cli.IntFlag{
			Name:   MaxIPsPerSessionFlag,
			Usage:  "max distinct client IPs per session per (torrent, path) within a 60s window (0 = unlimited)",
			Value:  5,
			EnvVar: "MAX_IPS_PER_SESSION",
		},
	)
}

// SessionLimiter limits concurrent requests per session and per
// (session, infohash, path) triple. Zero values mean unlimited.
// Scoping by path (not just infohash) lets HLS playback spread its
// segment requests across many counters while a download accelerator
// hammering a single file with parallel range requests hits one.
// A rolling-window distinct-IP cap per (session, torrent, path) catches
// shared-token abuse where the same token/file is fetched from many IPs.
type SessionLimiter struct {
	maxPerPath       int
	maxTotal         int
	maxIPsPerSession int

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type pathState struct {
	conc atomic.Int32
	mu   sync.Mutex
	ips  map[string]time.Time
}

type sessionState struct {
	total atomic.Int32
	mu    sync.Mutex
	paths map[string]*pathState
}

func NewSessionLimiter(c *cli.Context) *SessionLimiter {
	return &SessionLimiter{
		maxPerPath:       c.Int(MaxConcPerPathFlag),
		maxTotal:         c.Int(MaxConcTotalFlag),
		maxIPsPerSession: c.Int(MaxIPsPerSessionFlag),
		sessions:         make(map[string]*sessionState),
	}
}

func (l *SessionLimiter) Enabled() bool {
	return l.maxPerPath > 0 || l.maxTotal > 0 || l.maxIPsPerSession > 0
}

func (l *SessionLimiter) getSession(sessionID string) *sessionState {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[sessionID]
	if !ok {
		s = &sessionState{paths: make(map[string]*pathState)}
		l.sessions[sessionID] = s
	}
	return s
}

func (s *sessionState) getPath(key string) *pathState {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.paths[key]
	if !ok {
		p = &pathState{}
		s.paths[key] = p
	}
	return p
}

// subnetKey normalizes an IP to its subnet prefix (/24 for IPv4, /64 for IPv6)
// so that IPv6 privacy extensions and minor IPv4 NAT variations don't count as
// distinct IPs. Returns the raw string on parse failure (fail open).
func subnetKey(raw string) string {
	s := strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IP(v4.Mask(net.CIDRMask(24, 32))).String()
	}
	return net.IP(ip.Mask(net.CIDRMask(64, 128))).String()
}

// trackIP records the request IP (normalized to subnet), prunes expired
// entries, and returns the current distinct-subnet count within the window.
func (p *pathState) trackIP(ip string, window time.Duration) int {
	key := subnetKey(ip)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ips == nil {
		p.ips = make(map[string]time.Time)
	}
	now := time.Now()
	cutoff := now.Add(-window)
	p.ips[key] = now
	for k, t := range p.ips {
		if t.Before(cutoff) {
			delete(p.ips, k)
		}
	}
	return len(p.ips)
}

// Acquire tries to acquire a slot. Returns a release function on success,
// or nil with a reason string ("total", "path", "ips") on rejection.
func (l *SessionLimiter) Acquire(sessionID string, infoHash string, path string, ip string) (release func(), reason string) {
	if sessionID == "" {
		return func() {}, ""
	}

	s := l.getSession(sessionID)

	if l.maxTotal > 0 && int(s.total.Load()) >= l.maxTotal {
		return nil, "total"
	}

	pathKey := infoHash + "|" + path
	ps := s.getPath(pathKey)
	if l.maxPerPath > 0 && int(ps.conc.Load()) >= l.maxPerPath {
		return nil, "path"
	}

	if l.maxIPsPerSession > 0 && ip != "" {
		if ps.trackIP(ip, ipWindow) > l.maxIPsPerSession {
			return nil, "ips"
		}
	}

	s.total.Add(1)
	ps.conc.Add(1)

	return func() {
		ps.conc.Add(-1)
		newTotal := s.total.Add(-1)
		if newTotal <= 0 {
			l.mu.Lock()
			if s.total.Load() <= 0 {
				delete(l.sessions, sessionID)
			}
			l.mu.Unlock()
		}
	}, ""
}
