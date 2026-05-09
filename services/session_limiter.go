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
	MaxFilesPerHashFlag  = "max-files-per-hash"
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
			Name:   MaxFilesPerHashFlag,
			Usage:  "max distinct files (paths) concurrently active per session per torrent (0 = unlimited)",
			Value:  5,
			EnvVar: "MAX_FILES_PER_HASH",
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
// The per-hash files cap counts *distinct files* (paths) currently
// active for one (session, torrent) — orthogonal to per-path concurrency.
// It catches collection-style abuse (12+ different files of a 50-MP4
// series opened in parallel — what triggers tws OOMs) without penalising
// download accelerators that pile many connections onto a single file.
// A rolling-window distinct-IP cap per (session, torrent, path) catches
// shared-token abuse where the same token/file is fetched from many IPs.
type SessionLimiter struct {
	maxPerPath       int
	maxFilesPerHash  int
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

// hashState tracks which paths of one infohash are currently active for the
// session. activePaths maps path → in-flight request count; a path stays in
// the map (counts toward the file cap) until the last request on it ends.
type hashState struct {
	mu          sync.Mutex
	activePaths map[string]int
}

type sessionState struct {
	total  atomic.Int32
	mu     sync.Mutex
	paths  map[string]*pathState
	hashes map[string]*hashState
}

func NewSessionLimiter(c *cli.Context) *SessionLimiter {
	return &SessionLimiter{
		maxPerPath:       c.Int(MaxConcPerPathFlag),
		maxFilesPerHash:  c.Int(MaxFilesPerHashFlag),
		maxTotal:         c.Int(MaxConcTotalFlag),
		maxIPsPerSession: c.Int(MaxIPsPerSessionFlag),
		sessions:         make(map[string]*sessionState),
	}
}

func (l *SessionLimiter) Enabled() bool {
	return l.maxPerPath > 0 || l.maxFilesPerHash > 0 || l.maxTotal > 0 || l.maxIPsPerSession > 0
}

func (l *SessionLimiter) getSession(sessionID string) *sessionState {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[sessionID]
	if !ok {
		s = &sessionState{
			paths:  make(map[string]*pathState),
			hashes: make(map[string]*hashState),
		}
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

func (s *sessionState) getHash(infoHash string) *hashState {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[infoHash]
	if !ok {
		h = &hashState{activePaths: make(map[string]int)}
		s.hashes[infoHash] = h
	}
	return h
}

// tryAdd reserves a slot for path under the file cap. Returns true if the
// slot was granted and a release function. The caller must invoke release
// when the request finishes. Returns false if adding this path would
// exceed maxFiles. A path already active is always admitted (it just bumps
// the in-flight count for that path) — the cap counts distinct files,
// not requests.
func (h *hashState) tryAdd(path string, maxFiles int) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.activePaths[path]; !exists {
		if maxFiles > 0 && len(h.activePaths) >= maxFiles {
			return nil, false
		}
	}
	h.activePaths[path]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.activePaths[path]--
		if h.activePaths[path] <= 0 {
			delete(h.activePaths, path)
		}
	}, true
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
// or nil with a reason string ("total", "files", "path", "ips") on rejection.
func (l *SessionLimiter) Acquire(sessionID string, infoHash string, path string, ip string) (release func(), reason string) {
	if sessionID == "" {
		return func() {}, ""
	}

	s := l.getSession(sessionID)

	if l.maxTotal > 0 && int(s.total.Load()) >= l.maxTotal {
		return nil, "total"
	}

	hs := s.getHash(infoHash)
	releaseHash, ok := hs.tryAdd(path, l.maxFilesPerHash)
	if !ok {
		return nil, "files"
	}

	pathKey := infoHash + "|" + path
	ps := s.getPath(pathKey)
	if l.maxPerPath > 0 && int(ps.conc.Load()) >= l.maxPerPath {
		releaseHash()
		return nil, "path"
	}

	if l.maxIPsPerSession > 0 && ip != "" {
		if ps.trackIP(ip, ipWindow) > l.maxIPsPerSession {
			releaseHash()
			return nil, "ips"
		}
	}

	s.total.Add(1)
	ps.conc.Add(1)

	return func() {
		ps.conc.Add(-1)
		releaseHash()
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
