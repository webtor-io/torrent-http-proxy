package services

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

// harnessHash is the torrent every throttleHarness request asks for.
const harnessHash = "08ada5a7a6183aae1e09d831df6748d566095a10"

// heldBody answers 200 with size bytes, flushes them, and holds the
// response open until release is closed: a download still in progress.
func heldBody(size int, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, size))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
}

// releaser returns a func that closes ch once. It also runs at cleanup,
// ahead of the harness's own: a failed case must not leave an upstream
// holding a response open, or closing the servers waits on it forever.
func releaser(t *testing.T, ch chan struct{}) func() {
	f := sync.OnceFunc(func() { close(ch) })
	t.Cleanup(f)
	return f
}

// serveAsync runs r through proxyHTTP on another goroutine and delivers the
// closing log line's fields (nil when there is none) once it returns.
func (h *throttleHarness) serveAsync(t *testing.T, w http.ResponseWriter, r *http.Request) <-chan logrus.Fields {
	t.Helper()
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	done := make(chan logrus.Fields, 1)
	go func() {
		logger, hook := logtest.NewNullLogger()
		h.web.proxyHTTP(w, r, src, logrus.NewEntry(logger))
		for _, e := range hook.AllEntries() {
			switch e.Message {
			case "request served successfully", "bad request", "failed to serve request":
				done <- e.Data
				return
			}
		}
		done <- nil
	}()
	return done
}

// get requests the harness's torrent over HTTP, through the Web's own mux,
// and returns the status and body. httptest.ResponseRecorder takes a 1xx
// for the final status and then refuses the body; a real server does not.
func (h *throttleHarness) get(t *testing.T, claims jwt.MapClaims, external bool) (int, []byte) {
	t.Helper()
	return h.getPath(t, "/"+harnessHash+"/Sintel/Sintel.mkv", claims, external)
}

// getPath is get for another path.
func (h *throttleHarness) getPath(t *testing.T, path string, claims jwt.MapClaims, external bool) (int, []byte) {
	t.Helper()
	srv := newStatsServer(t, h.web)
	req, err := http.NewRequest(http.MethodGet, srv.URL+path+"?token="+signToken(t, h.secret, claims)+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if external {
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func (h *throttleHarness) statsEntry(session string) *sessionStatsEntry {
	return h.web.stats.lookup(statsKey(session, harnessHash))
}

func entryCount(s *SessionStats) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func limitedConns(e *sessionStatsEntry) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.limitedConns
}

// A download at the tier's limit shows its bytes and its limiter wait while
// it runs, not when it ends: a 2-hour download must have a speed.
func TestSessionStatsCountsLiveLimitedDownload(t *testing.T) {
	const session = "s-ss-live"
	claims := tierClaims("t-ss-live", session, "2M")
	release := make(chan struct{})
	h := newThrottleHarness(t, heldBody(64<<10, release))
	releaseBody := releaser(t, release)
	// ~6.5 ms of limiter wait for the 64 KiB.
	h.useThrottler(t, claims, &perByteThrottler{100 * time.Nanosecond})
	done := h.serveAsync(t, httptest.NewRecorder(), h.request(t, claims, true, "&download=true"))
	eventually(t, "the held response's bytes in the entry", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.bytes.Load() == 64<<10
	})
	e := h.statsEntry(session)
	eventually(t, "the held response's limiter wait in the entry", func() bool { return e.wait.Load() > 0 })
	if e.conns.Load() != 1 || limitedConns(e) != 1 {
		t.Errorf("conns %d, limited %d while open; want 1, 1", e.conns.Load(), limitedConns(e))
	}
	h.clock.Advance(3 * time.Second)
	if x := h.web.stats.sample(statsKey(session, harnessHash)); x.limitedOpen != int64(3*time.Second) || x.rate != "2M" {
		t.Errorf("limited open %v, rate %q; want 3s, 2M", time.Duration(x.limitedOpen), x.rate)
	}
	releaseBody()
	f := <-done
	if f == nil {
		t.Fatal("no closing log line")
	}
	// Both come from the one limiter the response had: the per-Write deltas
	// add up to exactly what the closing log line reports.
	if got, want := time.Duration(e.wait.Load()).Seconds(), seconds(t, f, "throttled"); got != want {
		t.Errorf("entry wait %v s, log throttled %v s", got, want)
	}
	if got := e.bytes.Load(); f["bytes"] != int(got) {
		t.Errorf("entry bytes %d, log bytes %v", got, f["bytes"])
	}
	if e.conns.Load() != 0 || limitedConns(e) != 0 {
		t.Errorf("conns %d, limited %d after the request ended; want 0, 0", e.conns.Load(), limitedConns(e))
	}
	h.clock.Advance(2 * time.Second)
	if x := h.web.stats.sample(statsKey(session, harnessHash)); x.limitedOpen != int64(3*time.Second) {
		t.Errorf("limited open grew to %v after the request ended, want 3s", time.Duration(x.limitedOpen))
	}
}

// Without a limiter the request is counted, but accrues no limited time, so
// the stream leaves throttled out; the rate claim is reported either way.
func TestSessionStatsUnlimitedRequest(t *testing.T) {
	cases := []struct {
		name     string
		claims   jwt.MapClaims
		limitOff bool
		rate     string
	}{
		{"token without rate", jwt.MapClaims{"role": "t-ss-norate", "sessionID": "s-ss-norate"}, false, ""},
		{"bandwidth limit off", tierClaims("t-ss-off", "s-ss-off", "2M"), true, "2M"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			session := c.claims["sessionID"].(string)
			release := make(chan struct{})
			h := newThrottleHarness(t, heldBody(1000, release))
			releaseBody := releaser(t, release)
			h.web.bandwidthLimit = !c.limitOff
			done := h.serveAsync(t, httptest.NewRecorder(), h.request(t, c.claims, true, ""))
			eventually(t, "the held response's bytes in the entry", func() bool {
				e := h.statsEntry(session)
				return e != nil && e.bytes.Load() == 1000
			})
			h.clock.Advance(3 * time.Second)
			x := h.web.stats.sample(statsKey(session, harnessHash))
			releaseBody()
			f := <-done
			if _, ok := f["throttled"]; ok {
				t.Fatalf("a limiter was installed, the case proves nothing")
			}
			if x.conns != 1 || x.limitedOpen != 0 || x.wait != 0 || x.rate != c.rate {
				t.Errorf("sample %+v, want conns 1, no limited time, no wait, rate %q", x, c.rate)
			}
		})
	}
}

// graceSegmentClaims is web-ui's grace segment token (GraceClaims) as it
// will be once it carries the session: bound to the harness's torrent, 50M.
func graceSegmentClaims(session string) jwt.MapClaims {
	return jwt.MapClaims{"role": "grace", "kind": "grace", "rate": "50M", "hash": harnessHash, "sessionID": session}
}

// A grace segment on the key of the session's playlist polls: its bytes and
// its time open count, its rate and its limiter's wait do not. rate stays
// the tier's with a grace segment the latest request, and throttled holds
// only the tier's wait, so a binding grace bucket never reads as the tier
// binding (web-ui: throttled ≥ 0.5 and use ≥ 0.9 of rate).
func TestSessionStatsGraceSegmentKeepsTierRateAndWait(t *testing.T) {
	const session = "s-ss-grace"
	tier := tierClaims("nobody", session, "5M")
	grace := graceSegmentClaims(session)
	release := make(chan struct{})
	h := newThrottleHarness(t, heldBody(64<<10, release))
	releaseBody := releaser(t, release)
	// Both limiters bind: ~3 ms of wait for the tier's 64 KiB, ~6.5 ms for
	// grace's.
	h.useThrottler(t, tier, &perByteThrottler{50 * time.Nanosecond})
	h.useThrottler(t, grace, &perByteThrottler{100 * time.Nanosecond})

	tierDone := h.serveAsync(t, httptest.NewRecorder(), h.request(t, tier, true, ""))
	eventually(t, "the tier request's bytes in the entry", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.bytes.Load() == 64<<10
	})
	// The grace segment starts after it: the latest request of the key.
	graceDone := h.serveAsync(t, httptest.NewRecorder(), h.request(t, grace, true, ""))
	e := h.statsEntry(session)
	eventually(t, "both requests' bytes in one entry", func() bool { return e.bytes.Load() == 128<<10 })
	if e.conns.Load() != 2 || limitedConns(e) != 1 {
		t.Errorf("conns %d, limited %d while both are open; want 2, 1 (the grace segment is not the tier's)",
			e.conns.Load(), limitedConns(e))
	}
	h.clock.Advance(3 * time.Second)
	if x := h.web.stats.sample(statsKey(session, harnessHash)); x.rate != "5M" || x.limitedOpen != int64(3*time.Second) {
		t.Errorf("rate %q, limited open %v with the grace segment the latest request; want 5M, 3s",
			x.rate, time.Duration(x.limitedOpen))
	}
	releaseBody()
	ft, fg := <-tierDone, <-graceDone
	if ft == nil || fg == nil {
		t.Fatal("no closing log line")
	}
	// The grace segment had a limiter and waited in it, or the case proves
	// nothing about its wait.
	if seconds(t, fg, "throttled") <= 0 {
		t.Fatal("the grace segment's limiter never held it")
	}
	if got, want := time.Duration(e.wait.Load()).Seconds(), seconds(t, ft, "throttled"); got != want {
		t.Errorf("entry wait %v s, want the tier request's alone (%v s; grace waited %v s)",
			got, want, seconds(t, fg, "throttled"))
	}
	if e.conns.Load() != 0 || limitedConns(e) != 0 {
		t.Errorf("conns %d, limited %d after both ended; want 0, 0", e.conns.Load(), limitedConns(e))
	}
}

// Only a session's own content counts: not what services fetch on its
// behalf, not status streams, not tokens without a session.
func TestSessionStatsSkipsNonContent(t *testing.T) {
	eventStream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}
	// 103 Early Hints comes first with its own headers; the response it
	// precedes is an event stream all the same.
	hintedEventStream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		eventStream(w, r)
	}
	cases := []struct {
		name     string
		upstream http.HandlerFunc
		claims   jwt.MapClaims
		external bool
	}{
		{"internal request", fixedBody(http.StatusOK, 1000), tierClaims("t-ss-int", "s-ss-int", "2M"), false},
		{"event stream", eventStream, tierClaims("t-ss-sse", "s-ss-sse", "2M"), true},
		{"event stream after 103", hintedEventStream, tierClaims("t-ss-103", "s-ss-103", "2M"), true},
		{"token without session (grace)", fixedBody(http.StatusOK, 1000), jwt.MapClaims{"role": "t-ss-grace", "rate": "2M"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newThrottleHarness(t, c.upstream)
			if status, body := h.get(t, c.claims, c.external); status != http.StatusOK || len(body) == 0 {
				t.Fatalf("got %d with %d bytes: the case proves nothing", status, len(body))
			}
			if n := entryCount(h.web.stats); n != 0 {
				t.Errorf("%d session stats entries, want none", n)
			}
		})
	}
}

// Only content counts. An error carries none, and counting it would let one
// token fill the map with requests for made-up hashes, each answered 404.
func TestSessionStatsCountsOnlySuccess(t *testing.T) {
	cases := []struct {
		status  int
		counted bool
	}{
		{http.StatusOK, true},
		{http.StatusPartialContent, true},
		{http.StatusNotModified, false},
		{http.StatusNotFound, false},
		{http.StatusRequestedRangeNotSatisfiable, false},
		{http.StatusBadGateway, false},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			session := "s-ss-status-" + strconv.Itoa(c.status)
			h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				if c.status != http.StatusNotModified {
					_, _ = w.Write(make([]byte, 1000))
				}
			})
			if status, _ := h.get(t, tierClaims("t-ss-status", session, "2M"), true); status != c.status {
				t.Fatalf("got %d, want %d: the case proves nothing", status, c.status)
			}
			if e := h.statsEntry(session); (e != nil) != c.counted {
				t.Errorf("entry %v after a %d, want counted %v", e, c.status, c.counted)
			}
		})
	}
}

// The stream answers for 40-hex infohashes only. URLParser takes any first
// segment with 5 hex digits in it, and a key under one would be held for a
// minute and never read.
func TestSessionStatsSkipsNonInfoHashPaths(t *testing.T) {
	for _, path := range []string{
		"/zzzzzabcdezzzzz/x",
		"/" + harnessHash + harnessHash[:24] + "/x", // 64 hex
		"/" + harnessHash[:39] + "/x",
	} {
		t.Run(path, func(t *testing.T) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
			if status, body := h.getPath(t, path, tierClaims("t-ss-junk", "s-ss-junk", "2M"), true); status != http.StatusOK || len(body) != 1000 {
				t.Fatalf("got %d with %d bytes: the case proves nothing", status, len(body))
			}
			if n := entryCount(h.web.stats); n != 0 {
				t.Errorf("%d session stats entries for %s, want none", n, path)
			}
		})
	}
}

// The map's key is a copy: the infohash proxyHTTP hands over is a slice of
// the request line, path and query whole, which ingress lets run to 4 MiB,
// and the map keeps the key for a minute after the request.
func TestSessionStatsKeyDoesNotPinRequest(t *testing.T) {
	const session = "s-ss-pin"
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	r := h.request(t, tierClaims("t-ss-pin", session, "2M"), true, "&pad="+strings.Repeat("x", 64<<10))
	line := r.URL.Path
	if src, err := h.web.parser.Parse(r.URL); err != nil || !aliases(src.InfoHash, line) {
		t.Fatalf("the parsed infohash is not a slice of the path (%v): the case proves nothing", err)
	}
	h.serve(t, httptest.NewRecorder(), r)
	s := h.web.stats
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) != 1 {
		t.Fatalf("%d entries, want 1", len(s.entries))
	}
	for k := range s.entries {
		if aliases(k.infoHash, line) {
			t.Errorf("the map's key points into the request path")
		}
	}
}

// Every visitor of an embed carries its owner's sessionID. Their content
// is kept apart from the owner's by the token's domain claim.
func TestSessionStatsKeyedByDomain(t *testing.T) {
	const session = "s-ss-embed-owner"
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	visitor := tierClaims("t-ss-embed", session, "2M")
	visitor["domain"] = "embed.example"
	if status, _ := h.get(t, visitor, true); status != http.StatusOK {
		t.Fatalf("got %d, want 200", status)
	}
	if h.statsEntry(session) != nil {
		t.Errorf("an embed visitor's content landed in the owner's own key")
	}
	if h.web.stats.lookup(sessionStatsKey{sessionID: session, domain: "embed.example", infoHash: harnessHash}) == nil {
		t.Fatalf("the embed visitor's content is not under its domain")
	}
	srv := newStatsServer(t, h.web)
	// rate comes from the key's entry: present only where the content is.
	owner := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, h.secret, session)))
	if ev := owner.next(t); ev["rate"] != nil {
		t.Errorf("the owner's stream reads the embed's content: %v", ev)
	}
	embedClaims := statsClaims(session, harnessHash)
	embedClaims["domain"] = "embed.example"
	embed := openStats(t, statsURL(srv.URL, harnessHash, signToken(t, h.secret, embedClaims)))
	if ev := embed.next(t); ev["rate"] != "2M" {
		t.Errorf("the embed domain's stream misses its content: %v", ev)
	}
}

// A viewer who seeks or closes the tab aborts the request mid-body. Under a
// real server ReverseProxy then panics with http.ErrAbortHandler, which a
// ResponseRecorder never sees; conns must come back down all the same.
func TestSessionStatsAbortReleasesConns(t *testing.T) {
	const session = "s-ss-abort"
	claims := tierClaims("t-ss-abort", session, "2M")
	release := make(chan struct{})
	h := newThrottleHarness(t, heldBody(64<<10, release))
	releaser(t, release)
	h.useThrottler(t, claims, &perByteThrottler{})
	srv := newStatsServer(t, h.web)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+harnessHash+"/Sintel/Sintel.mkv?token="+signToken(t, h.secret, claims)+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp := make(chan *http.Response, 1)
	go func() {
		defer close(resp)
		if r, err := http.DefaultClient.Do(req); err == nil {
			resp <- r
		}
	}()
	// The body stays open until the abort: closing it would end the
	// request before conns could be seen at 1.
	t.Cleanup(func() {
		if r, ok := <-resp; ok {
			_ = r.Body.Close()
		}
	})
	eventually(t, "conns 1", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.conns.Load() == 1
	})
	e := h.statsEntry(session)
	cancel()
	eventually(t, "conns and limited conns back to 0 after the abort", func() bool {
		return e.conns.Load() == 0 && limitedConns(e) == 0
	})
}

// Content after 103 Early Hints is still counted: the interim response
// defers the decision, it does not skip it.
func TestSessionStatsCountsContentAfterEarlyHints(t *testing.T) {
	const session = "s-ss-hint"
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1000))
	})
	if status, body := h.get(t, tierClaims("t-ss-hint", session, "2M"), true); status != http.StatusOK || len(body) != 1000 {
		t.Fatalf("got %d with %d bytes, want 200 with 1000", status, len(body))
	}
	if e := h.statsEntry(session); e == nil || e.bytes.Load() != 1000 {
		t.Fatalf("content after 103 not counted: entry %v", e)
	}
}

// conns counts the requests open right now: it goes up as they start and
// back down as each one ends.
func TestSessionStatsConnsLifecycle(t *testing.T) {
	const session = "s-ss-conns"
	claims := tierClaims("t-ss-conns", session, "2M")
	holds := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		heldBody(10, holds[r.URL.Query().Get("hold")])(w, r)
	})
	h.useThrottler(t, claims, &perByteThrottler{})
	releaseA, releaseB := releaser(t, holds["a"]), releaser(t, holds["b"])
	conns := func() int64 {
		if e := h.statsEntry(session); e != nil {
			return e.conns.Load()
		}
		return -1
	}
	doneA := h.serveAsync(t, httptest.NewRecorder(), h.request(t, claims, true, "&hold=a"))
	eventually(t, "conns 1", func() bool { return conns() == 1 })
	doneB := h.serveAsync(t, httptest.NewRecorder(), h.request(t, claims, true, "&hold=b"))
	eventually(t, "conns 2", func() bool { return conns() == 2 })
	releaseA()
	<-doneA
	if got := conns(); got != 1 {
		t.Errorf("conns = %d after one of two ended, want 1", got)
	}
	releaseB()
	<-doneB
	if got := conns(); got != 0 {
		t.Errorf("conns = %d after both ended, want 0", got)
	}
}

// A full map leaves the request unaccounted, never unserved.
func TestSessionStatsFullMapStillServes(t *testing.T) {
	claims := tierClaims("t-ss-full", "s-ss-full", "2M")
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
	h.useThrottler(t, claims, &perByteThrottler{})
	h.web.stats.maxEntries = 0
	rec := httptest.NewRecorder()
	h.serve(t, rec, h.request(t, claims, true, ""))
	if rec.Code != http.StatusOK || rec.Body.Len() != 1<<20 {
		t.Errorf("got %d with %d bytes, want 200 with %d", rec.Code, rec.Body.Len(), 1<<20)
	}
}

// ---- GET /session-stats/<infohash> ----

func signToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

const statsSecret = "session-stats-secret"

// newStatsServer serves a Web's mux over HTTP. Web.Close runs before the
// server's own Close, which would otherwise wait on open streams forever.
func newStatsServer(t *testing.T, web *Web) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(web.newMux())
	t.Cleanup(srv.Close)
	t.Cleanup(web.Close)
	return srv
}

// newStatsWeb is a Web with just what the session-stats handler uses.
func newStatsWeb(t *testing.T) (*Web, *fakeClock, *httptest.Server) {
	t.Helper()
	clk := newFakeClock()
	web := &Web{
		claims:  &Claims{apiKey: "k", apiSecret: statsSecret},
		stats:   newSessionStats(clk),
		closing: make(chan struct{}),
		gs:      cs.NewGracefulServer(time.Second),
	}
	return web, clk, newStatsServer(t, web)
}

func statsURL(base, hash, token string) string {
	return base + "/session-stats/" + hash + "?token=" + token + "&api-key=k"
}

// statsClient gives up on a stream whose headers never arrive (a handler
// that does not flush) instead of hanging the test binary.
var statsClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}}

type statsStream struct {
	resp   *http.Response
	events chan map[string]any
	cancel context.CancelFunc
}

// openStats GETs url; on 200 it reads "data: <json>\n\n" events in the
// background, and closes events when the stream ends.
func openStats(t *testing.T, url string) *statsStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := statsClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &statsStream{resp: resp, events: make(chan map[string]any, 16), cancel: cancel}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		close(s.events)
		return s
	}
	go func() {
		defer close(s.events)
		defer func() { _ = resp.Body.Close() }()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			blank, err := br.ReadString('\n')
			if err != nil || blank != "\n" || !strings.HasPrefix(line, "data: ") {
				t.Errorf("malformed event %q followed by %q", line, blank)
				return
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Errorf("event %q: %v", line, err)
				return
			}
			s.events <- ev
		}
	}()
	return s
}

func (s *statsStream) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("stream ended, want another event")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
	}
	return nil
}

func (s *statsStream) expectEnd(t *testing.T) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if ok {
			t.Fatalf("got event %v, want the stream to end", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after 5 s")
	}
}

// field returns a number from an event, failing when absent.
func field(t *testing.T, ev map[string]any, key string) float64 {
	t.Helper()
	v, ok := ev[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v in %v, want a number", key, ev[key], ev)
	}
	return v
}

// boolField returns a boolean from an event, failing when absent.
func boolField(t *testing.T, ev map[string]any, key string) bool {
	t.Helper()
	v, ok := ev[key].(bool)
	if !ok {
		t.Fatalf("%s = %#v in %v, want a boolean", key, ev[key], ev)
	}
	return v
}

func openStreams(s *SessionStats) int {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	return s.streamsTotal
}

// statsClaims are the claims of the token web-ui mints for the stream: its
// ordinary viewer claims, bound to the torrent by the standard hash claim.
func statsClaims(session, hash string) jwt.MapClaims {
	return jwt.MapClaims{
		"sessionID": session,
		"role":      "free",
		"rate":      "5M",
		"hash":      hash,
		"exp":       time.Now().Add(24 * time.Hour).Unix(),
	}
}

func statsToken(t *testing.T, secret, session string) string {
	t.Helper()
	return signToken(t, secret, statsClaims(session, harnessHash))
}

// Export URLs, and the tokens in them, reach browsers. The stream opens only
// for a token bound to its torrent by the standard hash claim, with a session
// and an expiry. Anything else is 403, and no answer carries CORS: the
// reader is web-ui's backend.
func TestSessionStatsAuth(t *testing.T) {
	_, _, srv := newStatsWeb(t)
	sign := func(c jwt.MapClaims) string { return signToken(t, statsSecret, c) }
	// with is the good token with one change.
	with := func(change func(jwt.MapClaims)) string {
		c := statsClaims("s1", harnessHash)
		change(c)
		return sign(c)
	}
	good := sign(statsClaims("s1", harnessHash))
	u := func(tok string) string { return statsURL(srv.URL, harnessHash, tok) }
	cases := []struct {
		name string
		url  string
		want int
	}{
		{"valid", u(good), http.StatusOK},
		{"hash claim in upper case", u(with(func(c jwt.MapClaims) { c["hash"] = strings.ToUpper(harnessHash) })), http.StatusOK},
		{"bad signature", u(signToken(t, "other-secret", statsClaims("s1", harnessHash))), http.StatusForbidden},
		{"no token", srv.URL + "/session-stats/" + harnessHash + "?api-key=k", http.StatusForbidden},
		{"wrong api key", srv.URL + "/session-stats/" + harnessHash + "?token=" + good + "&api-key=x", http.StatusForbidden},
		{"expired", u(with(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() })), http.StatusForbidden},
		// What a browser holds: the token of an export URL.
		{"export token", u(sign(jwt.MapClaims{"sessionID": "s1", "role": "free", "rate": "5M"})), http.StatusForbidden},
		{"no hash claim", u(with(func(c jwt.MapClaims) { delete(c, "hash") })), http.StatusForbidden},
		{"hash claim of another torrent", u(with(func(c jwt.MapClaims) { c["hash"] = strings.Repeat("a", 40) })), http.StatusForbidden},
		{"no sessionID", u(with(func(c jwt.MapClaims) { delete(c, "sessionID") })), http.StatusForbidden},
		{"empty sessionID", u(with(func(c jwt.MapClaims) { c["sessionID"] = "" })), http.StatusForbidden},
		{"no exp", u(with(func(c jwt.MapClaims) { delete(c, "exp") })), http.StatusForbidden},
		// The jwt library refuses these two itself now; thp's own check is
		// pinned without the library in TestSessionStatsTokenSession.
		{"exp not a number", u(with(func(c jwt.MapClaims) { c["exp"] = "never" })), http.StatusForbidden},
		// The stream would end at once.
		{"exp this second", u(with(func(c jwt.MapClaims) { c["exp"] = time.Now().Unix() })), http.StatusForbidden},
		// The jwt library checks iat and nbf with no leeway: the contract
		// says to leave them out, since a web-ui clock ahead of thp's is a 403.
		{"iat in the past", u(with(func(c jwt.MapClaims) { c["iat"] = time.Now().Add(-time.Minute).Unix() })), http.StatusOK},
		{"iat ahead of thp's clock", u(with(func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Minute).Unix() })), http.StatusForbidden},
		{"nbf ahead of thp's clock", u(with(func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Minute).Unix() })), http.StatusForbidden},
		{"short infohash", statsURL(srv.URL, harnessHash[:39], good), http.StatusBadRequest},
		{"long infohash", statsURL(srv.URL, harnessHash+"0", good), http.StatusBadRequest},
		{"non-hex infohash", statsURL(srv.URL, "g"+harnessHash[1:], good), http.StatusBadRequest},
		{"trailing path", statsURL(srv.URL, harnessHash+"/x", good), http.StatusBadRequest},
		{"no infohash", statsURL(srv.URL, "", good), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openStats(t, c.url)
			s.cancel()
			if s.resp.StatusCode != c.want {
				t.Errorf("status %d, want %d", s.resp.StatusCode, c.want)
			}
			if got, ok := s.resp.Header["Access-Control-Allow-Origin"]; ok {
				t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
			}
		})
	}
}

// The stream's token is an ordinary viewer token bound to one torrent: on
// content it serves that torrent at the viewer's rate, like the export token
// of the same torrent, and nothing else.
func TestSessionStatsTokenIsAnOrdinaryBoundToken(t *testing.T) {
	for _, external := range []bool{true, false} {
		t.Run("external="+strconv.FormatBool(external), func(t *testing.T) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
			c := statsClaims("s-ss-bound", harnessHash)
			if status, body := h.get(t, c, external); status != http.StatusOK || len(body) != 1000 {
				t.Errorf("own torrent: %d with %d bytes, want 200 with 1000", status, len(body))
			}
			other := "/" + strings.Repeat("a", 40) + "/Sintel/Sintel.mkv"
			if status, _ := h.getPath(t, other, c, external); status != http.StatusForbidden {
				t.Errorf("another torrent: %d, want 403", status)
			}
		})
	}
}

// captureLog collects what the standard logger logs during the test; the
// "/" handler and the session-stats handler both log through it.
func captureLog(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := &logtest.Hook{}
	old := logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
	logrus.AddHook(hook)
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(old) })
	return hook
}

// A refused token of the wrong kind is logged without the query string: a
// session-stats token is still good for the stream until its exp, and log
// lines are read (and quoted) far more widely than the stream is.
func TestRefusedTokensAreNotLogged(t *testing.T) {
	cases := []struct {
		name    string
		claims  jwt.MapClaims
		path    string // "" is the content URL
		message string
	}{
		{"export token on the stream", tierClaims("t-ss-log", "s-ss-log", "2M"), "/session-stats/" + harnessHash, "not a session-stats token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hook := captureLog(t)
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
			path := c.path
			if path == "" {
				path = "/" + harnessHash + "/Sintel/Sintel.mkv"
			}
			if status, _ := h.getPath(t, path, c.claims, true); status != http.StatusForbidden {
				t.Fatalf("status %d, want 403: the case proves nothing", status)
			}
			tok := signToken(t, h.secret, c.claims) // HS256 over the same claims: the same string
			var found bool
			for _, e := range hook.AllEntries() {
				if e.Message != c.message {
					continue
				}
				found = true
				if e.Data["infohash"] == nil {
					t.Errorf("no infohash in %v", e.Data)
				}
				for k, v := range e.Data {
					if str := fmt.Sprint(v); strings.Contains(str, tok) || strings.Contains(str, "api-key=") {
						t.Errorf("field %s = %q carries the token or api-key", k, str)
					}
				}
			}
			if !found {
				t.Fatalf("no %q line logged", c.message)
			}
		})
	}
}

func TestSessionStatsStreamHeadersAndFirstEvent(t *testing.T) {
	web, _, srv := newStatsWeb(t)
	web.stats.acquire(statsKey("s1", harnessHash), "5M", true)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, statsSecret, "s1")))
	if s.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", s.resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache, no-store, no-transform",
		"X-Accel-Buffering": "no",
	} {
		if got := s.resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, ok := s.resp.Header["Access-Control-Allow-Origin"]; ok {
		t.Errorf("Access-Control-Allow-Origin = %q, want none: web-ui reads the stream server side", got)
	}
	// The clock never moves: the first event may not wait for a tick.
	ev := s.next(t)
	if field(t, ev, "window_sec") != sessionStatsWindowSec || field(t, ev, "conns") != 1 || ev["rate"] != "5M" {
		t.Errorf("first event %v, want window_sec %d, conns 1, rate 5M", ev, sessionStatsWindowSec)
	}
	// No previous event to compare with: open now is active.
	if !boolField(t, ev, "active") {
		t.Errorf("first event %v with a request open, want active", ev)
	}
	if _, ok := ev["throttled"]; ok {
		t.Errorf("first event has throttled %v: no window yet, want absent", ev["throttled"])
	}
}

// One event per tick, each over the samples of the last window.
func TestSessionStatsStreamEvents(t *testing.T) {
	web, clk, srv := newStatsWeb(t)
	key := statsKey("s1", harnessHash)
	s := openStats(t, statsURL(srv.URL, strings.ToUpper(harnessHash), statsToken(t, statsSecret, "s1")))
	ev := s.next(t)
	if field(t, ev, "conns") != 0 || field(t, ev, "bytes_per_sec") != 0 || boolField(t, ev, "active") {
		t.Errorf("event before any request %v, want zeros and inactive", ev)
	}
	if _, ok := ev["rate"]; ok {
		t.Errorf("rate %v before any request, want absent", ev["rate"])
	}
	e := web.stats.acquire(key, "5M", true)
	e.bytes.Add(5000)
	e.wait.Add(int64(250 * time.Millisecond))
	clk.Advance(time.Second)
	ev = s.next(t)
	if field(t, ev, "bytes_per_sec") != 5000 || field(t, ev, "conns") != 1 || ev["rate"] != "5M" || field(t, ev, "throttled") != 0.25 {
		t.Errorf("event %v, want 5000 B/s, conns 1, rate 5M, throttled 0.25", ev)
	}
	web.stats.release(e, true)
	clk.Advance(time.Second)
	ev = s.next(t)
	// The request's 0.25 s of wait over the window's 2 s, not over the 1 s
	// it was open.
	if field(t, ev, "bytes_per_sec") != 2500 || field(t, ev, "conns") != 0 || field(t, ev, "throttled") != 0.125 {
		t.Errorf("event %v, want 2500 B/s over 2 s, conns 0, throttled 0.125", ev)
	}
	for i := 0; i < sessionStatsWindowSec; i++ {
		clk.Advance(time.Second)
		ev = s.next(t)
		if i == 2 {
			// 5 s in, the window is 0..5 s and still holds the request
			// (0..1 s): the handler averages over window_sec, not less.
			if field(t, ev, "bytes_per_sec") != 1000 || field(t, ev, "throttled") != 0.05 {
				t.Errorf("t=5 event %v, want 1000 B/s, throttled 0.05", ev)
			}
		}
	}
	// 7 s in, the window is 2..7 s: the request ended at 1 s.
	if field(t, ev, "bytes_per_sec") != 0 || ev["rate"] != "5M" {
		t.Errorf("event %v, want 0 B/s and the last rate", ev)
	}
	if _, ok := ev["throttled"]; ok {
		t.Errorf("throttled %v with no limited request in the window, want absent", ev["throttled"])
	}
}

// A download's speed reaches the stream while the download runs: content
// through proxyHTTP and the stream meet on (session, infohash), whatever
// the case of the infohash on either side.
func TestSessionStatsStreamReportsLiveDownload(t *testing.T) {
	const session = "s-ss-e2e"
	claims := tierClaims("t-ss-e2e", session, "2M")
	write := make(chan struct{})
	release := make(chan struct{})
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		<-write
		heldBody(64<<10, release)(w, r)
	})
	releaseWrite, releaseBody := releaser(t, write), releaser(t, release)
	h.useThrottler(t, claims, &perByteThrottler{})
	srv := newStatsServer(t, h.web)
	tok := signToken(t, h.secret, claims)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, h.secret, session)))
	if ev := s.next(t); field(t, ev, "conns") != 0 {
		t.Fatalf("first event %v, want conns 0", ev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+strings.ToUpper(harnessHash)+"/Sintel/Sintel.mkv?token="+tok+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp := make(chan *http.Response, 1)
	go func() {
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			close(resp)
			return
		}
		resp <- r
	}()
	releaseWrite()
	eventually(t, "the download's bytes in the entry", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.bytes.Load() == 64<<10
	})
	h.clock.Advance(time.Second)
	ev := s.next(t)
	if field(t, ev, "bytes_per_sec") != 64<<10 || field(t, ev, "conns") != 1 || ev["rate"] != "2M" {
		t.Errorf("event %v, want 65536 B/s, conns 1, rate 2M", ev)
	}
	if th := field(t, ev, "throttled"); th < 0 || th > 1 {
		t.Errorf("throttled = %v, want a share in [0, 1]", th)
	}
	releaseBody()
	if r, ok := <-resp; ok {
		_ = r.Body.Close()
	}
}

// The bug of 2026-09-25: a paid viewer (no limiter) gets each HLS segment in
// well under a second, and conns, sampled once a second, read 0 in every
// event while bytes_per_sec showed 1–2 MB/s. A request that opens and closes
// between two ticks reads active in the next event, through the real mux;
// the event after an idle second does not, though its window still holds
// the bytes.
func TestSessionStatsStreamReportsShortRequestBetweenTicks(t *testing.T) {
	const session = "s-ss-short"
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	srv := newStatsServer(t, h.web)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, h.secret, session)))
	if ev := s.next(t); boolField(t, ev, "active") || field(t, ev, "conns") != 0 {
		t.Fatalf("first event %v, want inactive with conns 0", ev)
	}
	// No rate claim: no limiter, as for a paid viewer.
	paid := jwt.MapClaims{"role": "t-ss-short", "sessionID": session}
	if status, body := h.get(t, paid, true); status != http.StatusOK || len(body) != 1000 {
		t.Fatalf("got %d with %d bytes, want 200 with 1000", status, len(body))
	}
	// The client can have the body before the handler returns; the case is
	// a request already over when the stream samples.
	eventually(t, "the request to end", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.conns.Load() == 0
	})
	h.clock.Advance(time.Second)
	ev := s.next(t)
	if !boolField(t, ev, "active") || field(t, ev, "conns") != 0 || field(t, ev, "bytes_per_sec") != 1000 {
		t.Errorf("event after the request %v, want active, conns 0, 1000 B/s", ev)
	}
	h.clock.Advance(time.Second)
	ev = s.next(t)
	if boolField(t, ev, "active") || field(t, ev, "bytes_per_sec") != 500 {
		t.Errorf("event after an idle second %v, want inactive, 500 B/s over the 2 s", ev)
	}
}

// What the contract says of active and conns alike: a request counts from
// its final response headers, so one still waiting upstream for its first
// byte (content-transcoder's WaitForSegment before any header) is in
// neither. Counting it earlier would count 404s for made-up hashes. Once
// its response has come, it reads active: the harness does count it.
func TestSessionStatsStreamSkipsRequestAwaitingFirstByte(t *testing.T) {
	const session = "s-ss-ttfb"
	entered := make(chan struct{}, 1)
	respond := make(chan struct{})
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-respond:
		case <-r.Context().Done():
			return
		}
		fixedBody(http.StatusOK, 1000)(w, r)
	})
	releaseRespond := releaser(t, respond)
	srv := newStatsServer(t, h.web)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, h.secret, session)))
	s.next(t)
	paid := jwt.MapClaims{"role": "t-ss-ttfb", "sessionID": session}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+harnessHash+"/Sintel/Sintel.mkv?token="+signToken(t, h.secret, paid)+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the upstream")
	}
	h.clock.Advance(time.Second)
	if ev := s.next(t); boolField(t, ev, "active") || field(t, ev, "conns") != 0 {
		t.Errorf("event while the request awaits its first byte %v, want inactive with conns 0", ev)
	}
	releaseRespond()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eventually(t, "the request to end", func() bool {
		e := h.statsEntry(session)
		return e != nil && e.conns.Load() == 0
	})
	h.clock.Advance(time.Second)
	if ev := s.next(t); !boolField(t, ev, "active") {
		t.Errorf("event after the response %v, want active", ev)
	}
}

// Streams end when the server shuts down, so none of them holds up a drain.
func TestSessionStatsStreamEndsOnClose(t *testing.T) {
	web, clk, srv := newStatsWeb(t)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, statsSecret, "s1")))
	s.next(t)
	web.Close()
	s.expectEnd(t)
	eventually(t, "the stream slot to be released", func() bool { return openStreams(web.stats) == 0 })
	if n := clk.liveTickers(sessionStatsSweepEvery); n != 0 {
		t.Errorf("session stats janitor still ticking after Web.Close")
	}
	web.Close() // idempotent: the test's cleanup calls it again
}

// The token is checked when the stream opens, and the stream ends at its
// exp: a leaked token's exp bounds how long it can be watched, not only
// when it can be opened. web-ui mints a new one for every open.
func TestSessionStatsStreamEndsAtExp(t *testing.T) {
	web, _, srv := newStatsWeb(t)
	c := statsClaims("s1", harnessHash)
	exp := time.Now().Unix() + 2
	c["exp"] = exp
	tok := signToken(t, statsSecret, c)
	s := openStats(t, statsURL(srv.URL, harnessHash, tok))
	if s.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", s.resp.StatusCode)
	}
	// The fake clock never moves: nothing but exp ends this stream.
	s.next(t)
	s.expectEnd(t)
	if now := time.Now(); now.Before(time.Unix(exp, 0)) {
		t.Errorf("stream ended at %v, before the token's exp %v", now, time.Unix(exp, 0))
	}
	eventually(t, "the stream slot to be released", func() bool { return openStreams(web.stats) == 0 })
	if again := openStats(t, statsURL(srv.URL, harnessHash, tok)); again.resp.StatusCode != http.StatusForbidden {
		t.Errorf("reopen with the expired token: status %d, want 403", again.resp.StatusCode)
	}
}

// A viewer who leaves frees the stream's slot without waiting for a tick.
func TestSessionStatsStreamEndsOnDisconnect(t *testing.T) {
	web, _, srv := newStatsWeb(t)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, statsSecret, "s1")))
	s.next(t)
	if n := openStreams(web.stats); n != 1 {
		t.Fatalf("%d open streams, want 1", n)
	}
	s.cancel()
	eventually(t, "the stream slot to be released", func() bool { return openStreams(web.stats) == 0 })
}

// openStatsFor opens a stream for session on hash with a token for them and
// checks the status; on 200 it waits for the first event.
func openStatsFor(t *testing.T, srv *httptest.Server, session, hash string, want int) *statsStream {
	t.Helper()
	s := openStats(t, statsURL(srv.URL, hash, signToken(t, statsSecret, statsClaims(session, hash))))
	if s.resp.StatusCode != want {
		t.Fatalf("stream for %s on %s: status %d, want %d", session, hash[:4], s.resp.StatusCode, want)
	}
	if want == http.StatusOK {
		s.next(t)
	}
	return s
}

// Three caps: per (session, domain, infohash), what one token grants; per
// session, looser; per pod.
func TestSessionStatsStreamCaps(t *testing.T) {
	web, _, srv := newStatsWeb(t)
	web.stats.maxStreamsPerKey = 2
	web.stats.maxStreamsPerSession = 3
	web.stats.maxStreams = 5
	h1, h2, h3 := harnessHash, strings.Repeat("a", 40), strings.Repeat("b", 40)
	rejected := func(reason string) float64 {
		return counterValue(t, promSessionStatsStreamsRejected.WithLabelValues(reason))
	}
	gauge := gaugeValue(t, promSessionStatsStreams)
	torrent, session, global := rejected("torrent"), rejected("session"), rejected("global")
	a1 := openStatsFor(t, srv, "a", h1, http.StatusOK)
	openStatsFor(t, srv, "a", h1, http.StatusOK)
	openStatsFor(t, srv, "a", h1, http.StatusTooManyRequests)
	openStatsFor(t, srv, "a", h2, http.StatusOK)
	openStatsFor(t, srv, "a", h3, http.StatusTooManyRequests)
	openStatsFor(t, srv, "b", h1, http.StatusOK)
	openStatsFor(t, srv, "c", h1, http.StatusOK)
	openStatsFor(t, srv, "d", h1, http.StatusServiceUnavailable)
	for reason, before := range map[string]float64{"torrent": torrent, "session": session, "global": global} {
		if got := rejected(reason) - before; got != 1 {
			t.Errorf("rejected{reason=%s} grew by %v, want 1", reason, got)
		}
	}
	if got := gaugeValue(t, promSessionStatsStreams) - gauge; got != 5 {
		t.Errorf("streams gauge grew by %v, want 5", got)
	}
	a1.cancel()
	eventually(t, "a's first slot to be released", func() bool { return openStreams(web.stats) == 4 })
	// The key's slot and the session's both came back.
	openStatsFor(t, srv, "a", h1, http.StatusOK)
	if got := gaugeValue(t, promSessionStatsStreams) - gauge; got != 5 {
		t.Errorf("streams gauge is %v over the start, want 5", got)
	}
}

// A leaked session-stats token names one torrent. Whoever holds it can take
// that torrent's slots, and only those: the victim's streams for the
// session's other torrents still open.
func TestSessionStatsLeakedTokenCannotLockOutSession(t *testing.T) {
	_, _, srv := newStatsWeb(t)
	a, b := harnessHash, strings.Repeat("a", 40)
	for i := 0; i < sessionStatsMaxStreamsPerKey; i++ {
		openStatsFor(t, srv, "victim", a, http.StatusOK)
	}
	openStatsFor(t, srv, "victim", a, http.StatusTooManyRequests)
	openStatsFor(t, srv, "victim", b, http.StatusOK)
}

// NewWeb is what prod runs; every other Web here is a literal. A stream on a
// NewWeb-built Web ends on Close: stats and closing are both wired.
func TestNewWebWiresSessionStats(t *testing.T) {
	set := flag.NewFlagSet("thp-test", flag.ContinueOnError)
	for _, f := range RegisterWebFlags(nil) {
		f.Apply(set)
	}
	web := NewWeb(cli.NewContext(cli.NewApp(), set, nil), nil, nil, nil, &Claims{apiKey: "k", apiSecret: statsSecret}, nil, nil, nil, nil)
	srv := newStatsServer(t, web)
	s := openStats(t, statsURL(srv.URL, harnessHash, statsToken(t, statsSecret, "s1")))
	if s.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", s.resp.StatusCode)
	}
	s.next(t)
	web.Close()
	s.expectEnd(t)
	web.Close()
}
