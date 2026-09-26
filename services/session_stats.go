package services

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// GET /session-stats/<infohash> streams, as Server-Sent Events, what this pod
// is delivering to the caller's session for that torrent: the speed over the
// last window, the content requests open now, whether one was open at all
// since the previous event, the token's rate, and the time that traffic spent
// in the tier's limiter over the window's wall time.
//
// Per pod and in memory is enough: rest-api points every export URL of an
// infohash at its rendezvous home node, ingress there reaches the thp pod on
// the same node (internalTrafficPolicy Local), and web-ui opens this stream
// on that node's direct-domain host (the premium edge in front of paid
// export URLs buffers SSE). What another pod serves the session is not seen
// here, by design.
const (
	sessionStatsPath = "/session-stats/"
	// sessionStatsWindowSec is the span each event averages over.
	sessionStatsWindowSec = 5
	sessionStatsInterval  = time.Second
	// An entry outlives its last content request by sessionStatsIdleTTL,
	// many windows long, so a stream never loses bytes still in its window.
	sessionStatsIdleTTL    = 60 * time.Second
	sessionStatsSweepEvery = 10 * time.Second
	// sessionStatsMaxEntries bounds the map at ~250 B an entry, ~12 MiB:
	// acquire copies each key, so an entry never pins the request line its
	// infohash was sliced from. Full, the map stops taking new keys rather
	// than grow with a flood of sessions; keys already in it keep counting.
	sessionStatsMaxEntries = 50000
	// One session's share of the map. A token without a hash claim is good
	// for any infohash, so one token could otherwise fill the map alone and
	// leave every new viewer on the pod uncounted. A viewer touches a few.
	sessionStatsMaxEntriesPerSession = 32
	// Tabs of one viewer on one torrent, keyed like the counters (session,
	// domain, infohash): all that one token, bound to its torrent, can hold.
	// Beyond it, 429.
	sessionStatsMaxStreamsPerKey = 4
	// All streams of one session, whatever the torrent. Well above the
	// per-torrent cap, so a leaked token cannot lock the session out of the
	// torrents it does not name. Beyond it, 429.
	sessionStatsMaxStreamsPerSession = 32
	// All streams of the pod; beyond it, 503.
	sessionStatsMaxStreams = 5000
)

var (
	promSessionStatsStreams = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_session_stats_streams",
		Help: "HTTP Proxy open GET /session-stats streams",
	})
	promSessionStatsStreamsRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_session_stats_streams_rejected_total",
		Help: "HTTP Proxy GET /session-stats streams refused at a cap (session: 429, global: 503)",
	}, []string{"reason"})
	promSessionStatsEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_session_stats_entries",
		Help: "HTTP Proxy (session, infohash) keys held for GET /session-stats",
	})
	promSessionStatsEntriesDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_session_stats_entries_dropped_total",
		Help: "HTTP Proxy content requests left out of GET /session-stats because the key map (global) or the session's share of it (session) was full",
	}, []string{"reason"})
)

func init() {
	prometheus.MustRegister(promSessionStatsStreams)
	prometheus.MustRegister(promSessionStatsStreamsRejected)
	prometheus.MustRegister(promSessionStatsEntries)
	prometheus.MustRegister(promSessionStatsEntriesDropped)
}

// statsClock is the time source of SessionStats; tests drive a fake one.
type statsClock interface {
	Now() time.Time
	// NewTicker ticks every d until its stop func is called.
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

type realStatsClock struct{}

func (realStatsClock) Now() time.Time { return time.Now() }

func (realStatsClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// sessionStatsKey is what one stream reports on. domain is the token's
// domain claim: every visitor of an embed carries its owner's sessionID, and
// the domain keeps their traffic out of the owner's own status bar.
type sessionStatsKey struct {
	sessionID string
	domain    string
	infoHash  string // lower case
}

// sessionStatsSession is the owner of keys for the per-session share.
type sessionStatsSession struct {
	sessionID string
	domain    string
}

func (k sessionStatsKey) session() sessionStatsSession {
	return sessionStatsSession{sessionID: k.sessionID, domain: k.domain}
}

// sessionStatsEntry holds monotonic totals for one key; streams sample them
// and take differences. Times are nanoseconds since SessionStats.epoch.
type sessionStatsEntry struct {
	// Added on every Write of every counted response: atomics only.
	bytes atomic.Int64
	wait  atomic.Int64 // ns spent in the limiter's Wait
	// Changed once per request.
	conns      atomic.Int64 // open content requests; the refcount
	lastActive atomic.Int64 // when the last request ended
	// Requests ended, ever. It only grows, so every stream compares it with
	// its own previous sample and none takes another's news, as a flag the
	// reader clears would. A count, not lastActive: two ends in one clock
	// reading would look like none.
	ends atomic.Int64

	mu sync.Mutex
	// limitedOpen integrates limitedConns over time up to limitedAt: the
	// total time limiter-wrapped requests have been open, across requests.
	// A stream reads only whether it grew in its window: whether a limited
	// request was open at all, so that throttled has an answer.
	limitedConns int64
	limitedOpen  int64
	limitedAt    int64
	rate         string // of the most recent request not on a grace token
}

// limitedOpenAt is limitedOpen carried forward to now. e.mu held.
func (e *sessionStatsEntry) limitedOpenAt(now int64) int64 {
	return e.limitedOpen + e.limitedConns*(now-e.limitedAt)
}

// SessionStats accounts content this pod delivers per (session, infohash)
// and serves it to GET /session-stats streams.
type SessionStats struct {
	clock   statsClock
	epoch   time.Time
	idleTTL time.Duration

	mu                   sync.Mutex
	entries              map[sessionStatsKey]*sessionStatsEntry
	perSession           map[sessionStatsSession]int // entries held, by session
	maxEntries           int
	maxEntriesPerSession int

	streamsMu            sync.Mutex
	keyStreams           map[sessionStatsKey]int
	sessionStreams       map[string]int // by sessionID
	streamsTotal         int
	maxStreamsPerKey     int
	maxStreamsPerSession int
	maxStreams           int

	stop     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
}

func NewSessionStats() *SessionStats {
	return newSessionStats(realStatsClock{})
}

func newSessionStats(clock statsClock) *SessionStats {
	s := &SessionStats{
		clock:                clock,
		epoch:                clock.Now(),
		idleTTL:              sessionStatsIdleTTL,
		entries:              make(map[sessionStatsKey]*sessionStatsEntry),
		perSession:           make(map[sessionStatsSession]int),
		maxEntries:           sessionStatsMaxEntries,
		maxEntriesPerSession: sessionStatsMaxEntriesPerSession,
		keyStreams:           make(map[sessionStatsKey]int),
		sessionStreams:       make(map[string]int),
		maxStreamsPerKey:     sessionStatsMaxStreamsPerKey,
		maxStreamsPerSession: sessionStatsMaxStreamsPerSession,
		maxStreams:           sessionStatsMaxStreams,
		stop:                 make(chan struct{}),
		stopped:              make(chan struct{}),
	}
	tick, stopTick := clock.NewTicker(sessionStatsSweepEvery)
	go s.janitor(tick, stopTick)
	return s
}

// now is the time since epoch: monotonic on the real clock.
func (s *SessionStats) now() int64 {
	return int64(s.clock.Now().Sub(s.epoch))
}

func (s *SessionStats) janitor(tick <-chan time.Time, stopTick func()) {
	defer close(s.stopped)
	defer stopTick()
	for {
		select {
		case <-tick:
			s.sweep()
		case <-s.stop:
			return
		}
	}
}

// Close stops the janitor. Safe to call more than once.
func (s *SessionStats) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.stopped
}

// sweep drops the entries with no open request whose last one ended more
// than idleTTL ago.
func (s *SessionStats) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		// release stores lastActive before it decrements conns, so an
		// entry seen at 0 conns carries the time its last request ended.
		if e.conns.Load() == 0 && time.Duration(now-e.lastActive.Load()) > s.idleTTL {
			delete(s.entries, k)
			if s.perSession[k.session()]--; s.perSession[k.session()] <= 0 {
				delete(s.perSession, k.session())
			}
			promSessionStatsEntries.Dec()
		}
	}
}

// acquire counts a content request whose response has started: its bytes,
// its time open, its rate (the stream's, until a later request's) and,
// when limited, its limiter's wait. It returns nil, and the request goes
// uncounted, when the map or the session's share of it is full.
func (s *SessionStats) acquire(k sessionStatsKey, rate string, limited bool) *sessionStatsEntry {
	e := s.open(k)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rate = rate
	if limited {
		now := s.now()
		e.limitedOpen = e.limitedOpenAt(now)
		e.limitedAt = now
		e.limitedConns++
	}
	return e
}

// acquireGrace counts a grace segment (isGraceToken) whose response has
// started: its bytes and its time open, which are the viewer's, and not its
// rate or its limiter's wait, which are the grace window's. rate and
// throttled stay the tier's however the key's requests interleave: the
// grace bucket binding is not the tier binding, and a playlist poll on the
// primary token between two grace segments would flip rate between them.
// Its writer has no tw, so release takes it as unlimited too.
func (s *SessionStats) acquireGrace(k sessionStatsKey) *sessionStatsEntry {
	return s.open(k)
}

// open finds or makes k's entry and counts one more request open on it;
// nil when the map or the session's share of it is full.
func (s *SessionStats) open(k sessionStatsKey) *sessionStatsEntry {
	s.mu.Lock()
	e := s.entries[k]
	if e == nil {
		reason := ""
		switch {
		case len(s.entries) >= s.maxEntries:
			reason = "global"
		case s.perSession[k.session()] >= s.maxEntriesPerSession:
			reason = "session"
		}
		if reason != "" {
			s.mu.Unlock()
			promSessionStatsEntriesDropped.WithLabelValues(reason).Inc()
			return nil
		}
		// The map keeps the key for the idle TTL after the request ends.
		// The infohash is a slice of the request line, path and query
		// whole: copied, the entry holds its 40 bytes, not megabytes.
		k = sessionStatsKey{
			sessionID: strings.Clone(k.sessionID),
			domain:    strings.Clone(k.domain),
			infoHash:  strings.Clone(k.infoHash),
		}
		e = &sessionStatsEntry{}
		s.entries[k] = e
		s.perSession[k.session()]++
		promSessionStatsEntries.Inc()
	}
	// Under s.mu: from here on the janitor sees an open request.
	e.conns.Add(1)
	s.mu.Unlock()
	return e
}

// release ends a request acquire counted.
func (s *SessionStats) release(e *sessionStatsEntry, limited bool) {
	e.mu.Lock()
	now := s.now()
	if limited {
		e.limitedOpen = e.limitedOpenAt(now)
		e.limitedAt = now
		e.limitedConns--
	}
	e.mu.Unlock()
	e.lastActive.Store(now)
	// Before conns falls, and sample loads conns first: a sample that no
	// longer counts the request open sees it ended.
	e.ends.Add(1)
	e.conns.Add(-1)
}

func (s *SessionStats) lookup(k sessionStatsKey) *sessionStatsEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[k]
}

type statsTotals struct {
	ends        int64 // requests ended
	bytes       int64
	wait        int64 // ns
	limitedOpen int64 // ns
}

// statsSample is one reading of a key's entry. Totals only compare between
// samples of the same entry.
type statsSample struct {
	at    int64              // ns since epoch
	entry *sessionStatsEntry // nil: the key had none
	statsTotals
	conns int64
	rate  string
}

func (s *SessionStats) sample(k sessionStatsKey) statsSample {
	e := s.lookup(k)
	if e == nil {
		return statsSample{at: s.now()}
	}
	e.mu.Lock()
	now := s.now()
	x := statsSample{
		at:    now,
		entry: e,
		rate:  e.rate,
		statsTotals: statsTotals{
			limitedOpen: e.limitedOpenAt(now),
		},
	}
	e.mu.Unlock()
	x.bytes = e.bytes.Load()
	x.wait = e.wait.Load()
	x.conns = e.conns.Load()
	x.ends = e.ends.Load() // after conns: see release
	return x
}

// acquireStream takes a stream slot for k. On refusal it returns a nil
// release and the status to answer. The tight cap is per key, what the
// stream's token grants; the session's own is loose, so the slots one token
// takes leave the session's other torrents their streams.
func (s *SessionStats) acquireStream(k sessionStatsKey) (release func(), status int) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	reason := ""
	switch {
	case s.streamsTotal >= s.maxStreams:
		reason, status = "global", http.StatusServiceUnavailable
	case s.keyStreams[k] >= s.maxStreamsPerKey:
		reason, status = "torrent", http.StatusTooManyRequests
	case s.sessionStreams[k.sessionID] >= s.maxStreamsPerSession:
		reason, status = "session", http.StatusTooManyRequests
	}
	if reason != "" {
		promSessionStatsStreamsRejected.WithLabelValues(reason).Inc()
		return nil, status
	}
	s.keyStreams[k]++
	s.sessionStreams[k.sessionID]++
	s.streamsTotal++
	promSessionStatsStreams.Inc()
	return func() {
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		if s.keyStreams[k]--; s.keyStreams[k] == 0 {
			delete(s.keyStreams, k)
		}
		if s.sessionStreams[k.sessionID]--; s.sessionStreams[k.sessionID] == 0 {
			delete(s.sessionStreams, k.sessionID)
		}
		s.streamsTotal--
		promSessionStatsStreams.Dec()
	}, http.StatusOK
}

// statsRing keeps a stream's last samples, one per tick.
type statsRing struct {
	size    int
	samples []statsSample
}

func newStatsRing(size int) *statsRing {
	return &statsRing{size: size, samples: make([]statsSample, 0, size)}
}

func (r *statsRing) push(x statsSample) {
	if n := len(r.samples); n > 0 && r.samples[n-1].entry != x.entry {
		// The key's entry was dropped, and maybe made anew, since the last
		// tick: the totals are another entry's and do not compare. The
		// dropped one did nothing for the idle TTL before it went, far
		// longer than the ring spans, and everything a new one holds came
		// after the last tick, so the kept samples read as zeros.
		for i := range r.samples {
			r.samples[i].statsTotals = statsTotals{}
		}
	}
	if len(r.samples) == r.size {
		r.samples = append(r.samples[:0], r.samples[1:]...)
	}
	r.samples = append(r.samples, x)
}

// sessionStatsEvent is the JSON of one event, the contract web-ui reads.
type sessionStatsEvent struct {
	WindowSec   int     `json:"window_sec"`
	BytesPerSec float64 `json:"bytes_per_sec"`
	Conns       int64   `json:"conns"`
	// A content request of the key was open at some moment since the
	// previous event (see event). Always sent, false included: web-ui tells
	// an older proxy, which has no such field, by its absence.
	Active bool `json:"active"`
	// The rate claim of the latest request not on a grace token: the tier's
	// (see acquireGrace).
	Rate string `json:"rate,omitempty"`
	// The limiter wait of the key's requests, grace segments excepted,
	// summed, over the window's wall time, 0..1 (see event). nil: no limited
	// request was open in the window (no limiter at all, or nothing open),
	// which is not the same as 0.
	Throttled *float64 `json:"throttled,omitempty"`
}

// event averages over the span the ring covers: window_sec once it is full,
// less while the stream is younger. A single sample spans nothing.
//
// throttled is the limiter wait of all the key's requests but grace
// segments (acquireGrace), summed, over that span of wall time, clamped to
// 1. For one request that is the share of the time the limiter held it. It
// is not the share of time any request was held: the bucket is the
// session's, a dry bucket holds every request at once, and each one's wait
// counts, so N parallel requests held together for 1/N of the window
// already read 1. At the cap that is right (the tier binds
// them all); below it the sum overstates, which is why a reader also wants
// bytes_per_sec near the rate. The time requests were open would be the
// wrong denominator: ingress takes an HLS segment whole at the tier's pace,
// so over its own open time every segment reads ≈ 1 however slow the client,
// and N parallel ranges stalled mid-body split a binding tier into 1/(N+1).
// Open time only decides whether there is an answer: none when no limited
// request was open in the window, which is not the same as 0.
//
// active is about the time since the previous event, not the window: a
// request is open now, or one ended since the stream's previous sample.
// conns alone is a gauge read once a second, and a viewer without a limiter
// gets an HLS segment in a fraction of that: the reading almost never lands
// inside a request (2026-09-25, a paid viewer at 1.1–2.2 MB/s read conns 0
// in every event). Open means counted: from the response's start (see
// sessionStatsWriter), so a request still waiting for its first byte is in
// neither conns nor active. The first event has no previous one: open now.
// A new entry reads active from its first sample: push zeroed the kept
// totals, and an entry is born with a request open, whose end moves ends
// before conns falls.
func (r *statsRing) event() sessionStatsEvent {
	first, last := r.samples[0], r.samples[len(r.samples)-1]
	ev := sessionStatsEvent{WindowSec: sessionStatsWindowSec, Conns: last.conns, Rate: last.rate}
	ev.Active = last.conns > 0
	if n := len(r.samples); n > 1 && last.ends != r.samples[n-2].ends {
		ev.Active = true
	}
	span := time.Duration(last.at - first.at)
	if span <= 0 {
		return ev
	}
	ev.BytesPerSec = float64(last.bytes-first.bytes) / span.Seconds()
	if last.limitedOpen > first.limitedOpen {
		th := throttleRatio(time.Duration(last.wait-first.wait), span)
		ev.Throttled = &th
	}
	return ev
}

// sessionStatsWriter feeds one proxied response into its key's entry. The
// entry is resolved once, when the response starts (final WriteHeader or
// first Write), since only then are the status and Content-Type known. Only
// 2xx content counts: event streams (the seeder's ?stats=true, warmup) are
// status, not content, and an error carries none (counting it would let
// requests for made-up hashes, answered 404, fill the map). After that a
// Write costs two atomic adds and, under a limiter, one load.
type sessionStatsWriter struct {
	http.ResponseWriter
	stats *SessionStats
	key   sessionStatsKey
	// grace: the request is on a grace token. Its rate and tw are unset:
	// it counts through acquireGrace, bytes and time open only.
	grace bool
	rate  string
	tw    *ThrottledResponseWriter // nil: no limiter on the response, or grace
	// WriteHeader(1xx) also arrives here, from the Transport's readLoop
	// goroutine via ReverseProxy's Got1xxResponse, and touches no field;
	// every other WriteHeader and Write runs on the handler goroutine.
	started bool
	e       *sessionStatsEntry // nil: not counted
	waited  time.Duration      // tw.Waited() already added to e
}

func (w *sessionStatsWriter) start(statusCode int) {
	if w.started {
		return
	}
	w.started = true
	// Callers pass final statuses only; a 1xx never gets here.
	if statusCode >= 300 || isEventStream(w.Header()) {
		return
	}
	if w.grace {
		w.e = w.stats.acquireGrace(w.key)
		return
	}
	w.e = w.stats.acquire(w.key, w.rate, w.tw != nil)
}

func (w *sessionStatsWriter) WriteHeader(statusCode int) {
	// A 1xx (103 Early Hints) is not the response: ReverseProxy writes it
	// with the interim headers in place, and the final Content-Type is not
	// known yet. It must not reach start or any field (see above).
	if statusCode >= 200 {
		w.start(statusCode)
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *sessionStatsWriter) Write(p []byte) (int, error) {
	// A Write before any final WriteHeader is an implicit 200.
	w.start(http.StatusOK)
	n, err := w.ResponseWriter.Write(p)
	if w.e != nil {
		w.e.bytes.Add(int64(n))
		if w.tw != nil {
			waited := w.tw.Waited()
			w.e.wait.Add(int64(waited - w.waited))
			w.waited = waited
		}
	}
	return n, err
}

// done ends the request's share of the entry; deferred by proxyHTTP.
func (w *sessionStatsWriter) done() {
	if w.e != nil {
		w.stats.release(w.e, w.tw != nil)
	}
}

func (w *sessionStatsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *sessionStatsWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	f.Flush()
}

var (
	_ http.ResponseWriter = &sessionStatsWriter{}
	_ http.Hijacker       = &sessionStatsWriter{}
	_ http.Flusher        = &sessionStatsWriter{}
)

func isInfoHash(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 40 && err == nil
}

// tokenExpiry is the token's exp claim as the jwt library reads it: a JSON
// number of whole seconds. false when it is absent or not a number.
func tokenExpiry(claims jwt.MapClaims) (time.Time, bool) {
	switch exp := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0), true
	case json.Number:
		v, err := exp.Int64()
		return time.Unix(v, 0), err == nil
	}
	return time.Time{}, false
}

// sessionStatsTokenSession returns the sessionID of a session-stats token for
// infoHash (lower case) and when the token expires. reason, when sessionID is
// "", names the claim that is missing or wrong.
func sessionStatsTokenSession(claims jwt.MapClaims, infoHash string, now time.Time) (sessionID string, expires time.Time, reason string) {
	// The stream takes an ordinary token bound to the torrent by the standard
	// hash claim, which web-ui mints server side for its own reads. Export
	// tokens reach browsers and usually carry no hash: taken here, a leaked
	// one would show every torrent the session fetches. Bound, a token tells
	// about the one torrent its holder already knows.
	if bound, _ := claims["hash"].(string); !strings.EqualFold(bound, infoHash) {
		return "", time.Time{}, "hash"
	}
	// The jwt library checks exp only when present: required here, so a
	// leaked token does not work forever. And not at its last second: the
	// stream ends at exp, and one that would end before its first tick is no
	// stream. golang-jwt v4 refuses that second and a non-number exp itself;
	// the old library took both, and this check does not lean on either.
	expires, ok := tokenExpiry(claims)
	if !ok || !now.Before(expires) {
		return "", time.Time{}, "exp"
	}
	if sessionID, _ = claims["sessionID"].(string); sessionID == "" {
		return "", time.Time{}, "sessionID"
	}
	return sessionID, expires, ""
}

func (s *Web) handleSessionStats(w http.ResponseWriter, r *http.Request) {
	// No CORS: web-ui's backend is the only reader.
	logger := logrus.WithField("handler", "session-stats")
	infoHash := strings.TrimPrefix(r.URL.Path, sessionStatsPath)
	if !isInfoHash(infoHash) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	infoHash = strings.ToLower(infoHash)
	claims, err := s.claims.Get(r.URL.Query().Get("token"), r.URL.Query().Get("api-key"))
	if err != nil {
		// Mostly expired tokens (a reconnect with a stale one): not
		// actionable line by line.
		logger.WithError(err).Debug("failed to get claims")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	sessionID, expires, reason := sessionStatsTokenSession(claims, infoHash, time.Now())
	if sessionID == "" {
		// A valid token of the wrong kind: web-ui minting it wrong, or an
		// export token tried here.
		logger.WithFields(logrus.Fields{
			"infohash": infoHash,
			"reason":   reason,
		}).Warn("not a session-stats token")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	key := sessionStatsKey{sessionID: sessionID, domain: tokenDomain(claims), infoHash: infoHash}
	release, status := s.stats.acquireStream(key)
	if release == nil {
		w.WriteHeader(status)
		return
	}
	defer release()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store, no-transform")
	// ingress-nginx buffers responses on this host; without this the
	// events would sit in its buffer.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	tick, stopTick := s.stats.clock.NewTicker(sessionStatsInterval)
	defer stopTick()
	// The token is checked once, here: without this, a stream opened before
	// exp would run until the client left or the pod went, and a leaked
	// token's exp would bound only when it can be opened. Wall clock, as the
	// check was: exp is about the token, not about the counters' time.
	// web-ui mints a new token for each open and reopen.
	expired := time.NewTimer(time.Until(expires))
	defer expired.Stop()
	ring := newStatsRing(sessionStatsWindowSec + 1)
	for {
		ring.push(s.stats.sample(key))
		b, err := json.Marshal(ring.event())
		if err != nil {
			logger.WithError(err).Error("failed to marshal session stats")
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-tick:
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-expired.C:
			return
		}
	}
}
