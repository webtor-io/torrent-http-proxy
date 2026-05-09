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
	MaxConcPerPathFlag     = "max-conc-per-path"
	MaxBigFilesPerHashFlag = "max-big-files-per-hash"
	BigFileThresholdFlag   = "big-file-threshold-bytes"
	MaxConcTotalFlag       = "max-conc-total"
	MaxIPsPerSessionFlag   = "max-ips-per-session"
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
			Name:   MaxBigFilesPerHashFlag,
			Usage:  "max distinct big files (paths) concurrently active per session per torrent (0 = unlimited)",
			Value:  5,
			EnvVar: "MAX_BIG_FILES_PER_HASH",
		},
		cli.Int64Flag{
			Name:   BigFileThresholdFlag,
			Usage:  "files smaller than this many bytes don't count toward the big-files cap (0 = treat all files as big)",
			Value:  10 * 1024 * 1024,
			EnvVar: "BIG_FILE_THRESHOLD_BYTES",
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

// lightByExt is a fast-path whitelist of extensions known to be small
// auxiliary files: subtitles, manifests, posters/thumbnails. They never
// count toward the big-files cap, even on the very first (cold-cache)
// request — otherwise the player loading several language tracks at once
// could 429 itself before the response-side size cache warms up.
var lightByExt = map[string]struct{}{
	".srt": {}, ".vtt": {}, ".ass": {}, ".ssa": {},
	".sub": {}, ".idx": {}, ".smi": {}, ".sbv": {},
	".m3u8": {}, ".mpd": {},
	".jpg": {}, ".jpeg": {}, ".png": {}, ".webp": {}, ".gif": {},
	".json": {}, ".xml": {}, ".nfo": {}, ".txt": {},
}

func isLightExt(path string) bool {
	if i := strings.LastIndex(path, "."); i >= 0 {
		_, ok := lightByExt[strings.ToLower(path[i:])]
		return ok
	}
	return false
}

// SizeLookup returns the byte size of the file behind (infoHash, path) if
// it is known (typically populated lazily from upstream Content-Length on
// the first response). When unknown, the limiter pessimistically assumes
// the file is big — so unknown-extension uploads get capped until the
// cache learns their size.
type SizeLookup func(infoHash, path string) (sizeBytes int64, known bool)

// SessionLimiter limits concurrent requests per session and per
// (session, infohash, path) triple. Zero values mean unlimited.
// Scoping by path (not just infohash) lets HLS playback spread its
// segment requests across many counters while a download accelerator
// hammering a single file with parallel range requests hits one.
// The per-hash big-files cap counts *distinct big files* (paths) currently
// active for one (session, torrent) — orthogonal to per-path concurrency.
// "Big" is determined first by extension fast-path (subs, manifests,
// images are always light) and then by cached upstream Content-Length
// against bigFileThreshold. It catches collection-style abuse (12+ video
// files of a 50-MP4 series opened in parallel — what triggers tws OOMs)
// without penalising download accelerators piling many connections onto
// a single file or players loading several language tracks.
// A rolling-window distinct-IP cap per (session, torrent, path) catches
// shared-token abuse where the same token/file is fetched from many IPs.
type SessionLimiter struct {
	maxPerPath        int
	maxBigFilesPerHash int
	bigFileThreshold  int64
	maxTotal          int
	maxIPsPerSession  int

	sizeLookup SizeLookup

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type pathState struct {
	conc atomic.Int32
	mu   sync.Mutex
	ips  map[string]time.Time
}

// hashState tracks which big files of one infohash are currently active for
// the session. activeBigPaths maps path → in-flight request count for paths
// currently classified as big; a path stays in the map (counts toward the
// cap) until the last in-flight request on it ends. Light files are not
// tracked here at all.
type hashState struct {
	mu             sync.Mutex
	activeBigPaths map[string]int
}

type sessionState struct {
	total  atomic.Int32
	mu     sync.Mutex
	paths  map[string]*pathState
	hashes map[string]*hashState
}

func NewSessionLimiter(c *cli.Context) *SessionLimiter {
	return &SessionLimiter{
		maxPerPath:         c.Int(MaxConcPerPathFlag),
		maxBigFilesPerHash: c.Int(MaxBigFilesPerHashFlag),
		bigFileThreshold:   c.Int64(BigFileThresholdFlag),
		maxTotal:           c.Int(MaxConcTotalFlag),
		maxIPsPerSession:   c.Int(MaxIPsPerSessionFlag),
		sessions:           make(map[string]*sessionState),
	}
}

// SetSizeLookup wires the upstream-size cache the limiter consults to
// classify a path as big or light. Call once at startup; concurrent reads
// during normal operation use the cached value.
func (l *SessionLimiter) SetSizeLookup(f SizeLookup) {
	l.sizeLookup = f
}

func (l *SessionLimiter) Enabled() bool {
	return l.maxPerPath > 0 || l.maxBigFilesPerHash > 0 || l.maxTotal > 0 || l.maxIPsPerSession > 0
}

// isBigFile decides whether the given path counts toward the per-hash
// big-files cap. Extension whitelist short-circuits to false so the player
// never gets 429ed on a fresh subtitle/manifest load. Otherwise the
// upstream-size cache decides; an unknown size falls back to "big" so an
// abuser with many uncached unique paths still hits the cap.
func (l *SessionLimiter) isBigFile(infoHash, path string) bool {
	if isLightExt(path) {
		return false
	}
	if l.bigFileThreshold > 0 && l.sizeLookup != nil {
		if size, known := l.sizeLookup(infoHash, path); known {
			return size >= l.bigFileThreshold
		}
	}
	return true
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
		h = &hashState{activeBigPaths: make(map[string]int)}
		s.hashes[infoHash] = h
	}
	return h
}

// tryAddBig reserves a big-file slot. Returns a release function on success
// (caller must invoke when the request finishes), or false if adding this
// path as a *new* active big file would exceed maxBig. A path already in
// the active set always succeeds — the cap counts distinct files, not
// requests, so additional concurrent ranges of the same file pass freely.
func (h *hashState) tryAddBig(path string, maxBig int) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.activeBigPaths[path]; !exists {
		if maxBig > 0 && len(h.activeBigPaths) >= maxBig {
			return nil, false
		}
	}
	h.activeBigPaths[path]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.activeBigPaths[path]--
		if h.activeBigPaths[path] <= 0 {
			delete(h.activeBigPaths, path)
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
// or nil with a reason string ("total", "bigfiles", "path", "ips") on rejection.
func (l *SessionLimiter) Acquire(sessionID string, infoHash string, path string, ip string) (release func(), reason string) {
	if sessionID == "" {
		return func() {}, ""
	}

	s := l.getSession(sessionID)

	if l.maxTotal > 0 && int(s.total.Load()) >= l.maxTotal {
		return nil, "total"
	}

	var releaseHash func()
	if l.isBigFile(infoHash, path) {
		hs := s.getHash(infoHash)
		var ok bool
		releaseHash, ok = hs.tryAddBig(path, l.maxBigFilesPerHash)
		if !ok {
			return nil, "bigfiles"
		}
	} else {
		releaseHash = func() {}
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
