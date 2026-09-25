package services

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeClock is a statsClock the test moves by hand. Its tickers fire from
// Advance and, like time.Ticker, drop a tick the reader has not taken yet.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

type fakeTicker struct {
	every   time.Duration
	next    time.Time
	c       chan time.Time
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{every: d, next: c.now.Add(d), c: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, t)
	return t.c, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		t.stopped = true
	}
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.tickers {
		if t.stopped {
			continue
		}
		for !t.next.After(c.now) {
			select {
			case t.c <- c.now:
			default:
			}
			t.next = t.next.Add(t.every)
		}
	}
}

// liveTickers counts the tickers of period every that are not stopped.
func (c *fakeClock) liveTickers(every time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.tickers {
		if t.every == every && !t.stopped {
			n++
		}
	}
	return n
}

// newTestSessionStats is a SessionStats on a fake clock, closed with the test.
func newTestSessionStats(t *testing.T) (*SessionStats, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	s := newSessionStats(clk)
	t.Cleanup(s.Close)
	return s, clk
}

// eventually polls cond until it holds, failing the test after 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	d := &dto.Metric{}
	if err := c.Write(d); err != nil {
		t.Fatal(err)
	}
	return d.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	d := &dto.Metric{}
	if err := g.Write(d); err != nil {
		t.Fatal(err)
	}
	return d.GetGauge().GetValue()
}

// statsKey is the key of a token with a sessionID and no domain claim, as
// the tokens of these tests are.
func statsKey(session, hash string) sessionStatsKey {
	return sessionStatsKey{sessionID: session, domain: tokenDomain(nil), infoHash: hash}
}

// aliases reports whether a lies inside b's memory.
func aliases(a, b string) bool {
	pa, pb := uintptr(unsafe.Pointer(unsafe.StringData(a))), uintptr(unsafe.Pointer(unsafe.StringData(b)))
	return len(a) > 0 && pa >= pb && pa < pb+uintptr(len(b))
}

// smp is a sample at `at` from the stream's start.
func smp(at time.Duration, e *sessionStatsEntry, bytes int64, wait, open time.Duration) statsSample {
	return statsSample{
		at:          int64(at),
		entry:       e,
		statsTotals: statsTotals{bytes: bytes, wait: int64(wait), limitedOpen: int64(open)},
	}
}

func ptr(f float64) *float64 { return &f }

func TestStatsRingWindow(t *testing.T) {
	a, b := &sessionStatsEntry{}, &sessionStatsEntry{}
	s := time.Second
	cases := []struct {
		name          string
		samples       []statsSample
		wantBPS       float64
		wantThrottled *float64 // nil: absent
	}{
		{
			name:    "first sample has no window yet",
			samples: []statsSample{smp(0, a, 5000, s, 2*s)},
		},
		{
			name:    "fewer samples than the window average over the span they cover",
			samples: []statsSample{smp(0, a, 0, 0, 0), smp(s, a, 1000, 0, 0), smp(2*s, a, 3000, 0, 0)},
			wantBPS: 1500,
		},
		{
			name: "a full ring averages the last window_sec only",
			samples: []statsSample{
				smp(0, a, 0, 0, 0), smp(s, a, 1000, 0, 0), smp(2*s, a, 2000, 0, 0), smp(3*s, a, 3000, 0, 0),
				smp(4*s, a, 3000, 0, 0), smp(5*s, a, 3000, 0, 0), smp(6*s, a, 3000, 0, 0), smp(7*s, a, 3000, 0, 0),
			},
			wantBPS: 200, // (3000-2000) over 2..7 s
		},
		{
			name:          "throttled is limiter wait over the window's wall time",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(2*s, a, 4000, 500*time.Millisecond, 2*s)},
			wantBPS:       2000,
			wantThrottled: ptr(0.25),
		},
		{
			// HLS at 5M (640 KiB/s): ingress takes a segment of 1.9 s of
			// the rate whole. The bucket, full after the idle gap, lets the
			// first second of it through at once (capacity = 1 s of the
			// rate); the other 0.9 s waits. Then the player idles until it
			// wants the next one. Over the open time alone the segment
			// reads 0.9, whatever the client's speed.
			name: "a segment open 1 s of the window counts its wait over the whole window",
			samples: []statsSample{
				smp(0, a, 0, 0, 0), smp(s, a, 1216<<10, 900*time.Millisecond, s), smp(2*s, a, 1216<<10, 900*time.Millisecond, s),
				smp(3*s, a, 1216<<10, 900*time.Millisecond, s), smp(4*s, a, 1216<<10, 900*time.Millisecond, s), smp(5*s, a, 1216<<10, 900*time.Millisecond, s),
			},
			wantBPS:       float64(1216<<10) / 5,
			wantThrottled: ptr(0.18),
		},
		{
			// Four parallel ranges open the whole window, three stalled
			// mid-body, the fourth held by the limiter all along: the tier
			// binds. Over open time that is 5 s of 20, 0.25.
			name:          "stalled parallel ranges do not dilute a binding tier",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(5*s, a, 3<<20, 5*s, 20*s)},
			wantBPS:       float64(3<<20) / 5,
			wantThrottled: ptr(1),
		},
		{
			// The bucket is the session's: three requests at the cap each
			// wait most of the window, 12 s of wait in 5 s.
			name:          "parallel requests at the cap add up past 1 and clamp",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(5*s, a, 3<<20, 12*s, 15*s)},
			wantBPS:       float64(3<<20) / 5,
			wantThrottled: ptr(1),
		},
		{
			// A dry bucket holds all of the session's requests at once and
			// each one's wait counts: four held together for the same 0.5 s
			// read 0.4, where the time any of them was held is 0.1 of the
			// window. The sum is the contract; the union would take a count
			// of requests in Wait kept under a lock on every Write.
			name:          "parallel requests held together add up: the sum, not the union",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(5*s, a, 1<<20, 2*s, 20*s)},
			wantBPS:       float64(1<<20) / 5,
			wantThrottled: ptr(0.4),
		},
		{
			name:          "a client slower than the tier reads a small share",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(5*s, a, 1<<20, 400*time.Millisecond, 5*s)},
			wantBPS:       float64(1<<20) / 5,
			wantThrottled: ptr(0.08),
		},
		{
			name:          "throttled is clamped to 1",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(s, a, 0, 3*s, 2*s)},
			wantThrottled: ptr(1),
		},
		{
			name:          "a limited request that never waited reads 0, not absent",
			samples:       []statsSample{smp(0, a, 0, 0, 0), smp(s, a, 100, 0, s)},
			wantBPS:       100,
			wantThrottled: ptr(0),
		},
		{
			name:    "no limited request open in the window: throttled absent",
			samples: []statsSample{smp(0, a, 0, s, 4*s), smp(s, a, 100, s, 4*s)},
			wantBPS: 100,
		},
		{
			name:          "entry recreated with lower totals",
			samples:       []statsSample{smp(0, a, 10<<20, s, 4*s), smp(s, a, 10<<20, s, 4*s), smp(2*s, b, 3000, 500*time.Millisecond, s)},
			wantBPS:       1500,
			wantThrottled: ptr(0.25),
		},
		{
			name:    "entry recreated with higher totals",
			samples: []statsSample{smp(0, a, 1000, 0, 0), smp(s, b, 5000, 0, 0)},
			wantBPS: 5000,
		},
		{
			name:    "recreated entry keeps accruing",
			samples: []statsSample{smp(0, a, 10<<20, 0, 0), smp(s, b, 1000, 0, 0), smp(2*s, b, 3000, 0, 0)},
			wantBPS: 1500,
		},
		{
			name:    "entry dropped",
			samples: []statsSample{smp(0, a, 10<<20, s, 4*s), smp(s, nil, 0, 0, 0)},
		},
		{
			name:    "entry created after the stream opened",
			samples: []statsSample{smp(0, nil, 0, 0, 0), smp(s, nil, 0, 0, 0), smp(2*s, a, 4000, 0, 0)},
			wantBPS: 2000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newStatsRing(sessionStatsWindowSec + 1)
			for _, x := range c.samples {
				r.push(x)
			}
			ev := r.event()
			if ev.WindowSec != sessionStatsWindowSec {
				t.Errorf("window_sec = %d, want %d", ev.WindowSec, sessionStatsWindowSec)
			}
			if ev.BytesPerSec != c.wantBPS {
				t.Errorf("bytes_per_sec = %v, want %v", ev.BytesPerSec, c.wantBPS)
			}
			switch {
			case c.wantThrottled == nil && ev.Throttled != nil:
				t.Errorf("throttled = %v, want absent", *ev.Throttled)
			case c.wantThrottled != nil && ev.Throttled == nil:
				t.Errorf("throttled absent, want %v", *c.wantThrottled)
			case c.wantThrottled != nil && *ev.Throttled != *c.wantThrottled:
				t.Errorf("throttled = %v, want %v", *ev.Throttled, *c.wantThrottled)
			}
		})
	}
}

// act is a sample at `at` with conns requests open now and ends requests
// ended so far on e.
func act(at time.Duration, e *sessionStatsEntry, conns, ends int64) statsSample {
	x := statsSample{at: int64(at), entry: e, conns: conns}
	x.ends = ends
	return x
}

// active answers for the time since the previous event, not for the window:
// a request open now, or one that ended since the stream's last sample,
// however short it was. The first event has no previous one: open now.
func TestStatsRingActive(t *testing.T) {
	a, b := &sessionStatsEntry{}, &sessionStatsEntry{}
	s := time.Second
	cases := []struct {
		name    string
		samples []statsSample
		want    bool
	}{
		{"first event, a request open", []statsSample{act(0, a, 1, 0)}, true},
		{"first event, requests ended before the stream opened", []statsSample{act(0, a, 0, 5)}, false},
		{"first event, no entry", []statsSample{act(0, nil, 0, 0)}, false},
		// An HLS segment without a limiter: opened and closed between two
		// samples, conns never saw it.
		{"a request opened and ended between two events", []statsSample{act(0, a, 0, 0), act(s, a, 0, 1)}, true},
		{"a request open at the previous event ended since", []statsSample{act(0, a, 1, 0), act(s, a, 0, 1)}, true},
		{"a request open throughout", []statsSample{act(0, a, 1, 0), act(s, a, 1, 0)}, true},
		{"nothing open, nothing ended since the previous event", []statsSample{act(0, a, 0, 2), act(s, a, 0, 2)}, false},
		// bytes_per_sec still averages the request in; active does not.
		{"idle since the previous event, busy earlier in the window", []statsSample{act(0, a, 0, 0), act(s, a, 0, 3), act(2*s, a, 0, 3)}, false},
		// The samples of one tick: a span of 0 is no reason to skip it.
		{"the clock did not move", []statsSample{act(s, a, 0, 0), act(s, a, 0, 1)}, true},
		// A new entry holds only what came after the last sample, even
		// when its count equals the old entry's.
		{"entry recreated since the previous event", []statsSample{act(0, a, 0, 1), act(s, b, 0, 1)}, true},
		{"entry created since the previous event", []statsSample{act(0, nil, 0, 0), act(s, a, 0, 1)}, true},
		{"entry dropped", []statsSample{act(0, a, 0, 4), act(s, nil, 0, 0)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newStatsRing(sessionStatsWindowSec + 1)
			for _, x := range c.samples {
				r.push(x)
			}
			if got := r.event().Active; got != c.want {
				t.Errorf("active = %v, want %v", got, c.want)
			}
		})
	}
}

// The case of the bug: a request that opens and closes between two events
// (a segment fetched in well under a second) reads active in the next
// event, conns 0 and all; the event after that, with nothing since, does
// not. The fake clock does not move while the request runs, so a request
// that ends at the very instant of the previous sample must count too.
func TestSessionStatsActiveBetweenEvents(t *testing.T) {
	s, clk := newTestSessionStats(t)
	k := statsKey("s1", "h")
	r := newStatsRing(sessionStatsWindowSec + 1)
	next := func() sessionStatsEvent {
		clk.Advance(time.Second)
		r.push(s.sample(k))
		return r.event()
	}
	r.push(s.sample(k))
	if ev := r.event(); ev.Active {
		t.Errorf("first event %+v before any request: want inactive", ev)
	}
	e := s.acquire(k, "", false)
	e.bytes.Add(1000)
	s.release(e, false)
	if ev := next(); !ev.Active || ev.Conns != 0 {
		t.Errorf("event after a request opened and closed between ticks: %+v, want active with conns 0", ev)
	}
	if ev := next(); ev.Active || ev.BytesPerSec == 0 {
		t.Errorf("event after an idle second: %+v, want inactive while the window still holds the bytes", ev)
	}
	// Open at one event, gone before the next: still active in the next.
	e = s.acquire(k, "", false)
	if ev := next(); !ev.Active || ev.Conns != 1 {
		t.Errorf("event with a request open: %+v, want active, conns 1", ev)
	}
	s.release(e, false)
	if ev := next(); !ev.Active || ev.Conns != 0 {
		t.Errorf("event after the open request ended: %+v, want active, conns 0", ev)
	}
	if ev := next(); ev.Active {
		t.Errorf("event after an idle second: %+v, want inactive", ev)
	}
}

// Ends are counted, not timed: two requests that end in one clock reading,
// with a sample between them, are two ends. When the last end's time is
// compared instead, the second one looks like no change.
func TestSessionStatsActiveCountsEndsNotTimes(t *testing.T) {
	s, clk := newTestSessionStats(t)
	k := statsKey("s1", "h")
	r := newStatsRing(sessionStatsWindowSec + 1)
	clk.Advance(time.Second)
	s.release(s.acquire(k, "", false), false)
	r.push(s.sample(k))
	// The same clock reading: the sample and the next request share it.
	s.release(s.acquire(k, "", false), false)
	clk.Advance(time.Second)
	r.push(s.sample(k))
	if ev := r.event(); !ev.Active {
		t.Errorf("event after a second request ended in the previous sample's clock reading: %+v, want active", ev)
	}
}

// Up to four streams read one key (tabs of one viewer). Each compares with
// its own previous sample: the stream that samples first does not take the
// news from the others.
func TestSessionStatsActiveSeenByEveryStream(t *testing.T) {
	s, clk := newTestSessionStats(t)
	k := statsKey("s1", "h")
	a, b := newStatsRing(sessionStatsWindowSec+1), newStatsRing(sessionStatsWindowSec+1)
	a.push(s.sample(k))
	b.push(s.sample(k))
	s.release(s.acquire(k, "", false), false)
	clk.Advance(time.Second)
	a.push(s.sample(k))
	clk.Advance(100 * time.Millisecond)
	b.push(s.sample(k))
	if !a.event().Active || !b.event().Active {
		t.Errorf("active: stream a %v, stream b %v; want both true", a.event().Active, b.event().Active)
	}
	clk.Advance(900 * time.Millisecond)
	a.push(s.sample(k))
	clk.Advance(100 * time.Millisecond)
	b.push(s.sample(k))
	if a.event().Active || b.event().Active {
		t.Errorf("after an idle second: stream a %v, stream b %v; want both false", a.event().Active, b.event().Active)
	}
}

// conns and rate are the latest sample's, not window aggregates.
func TestStatsRingConnsAndRateAreCurrent(t *testing.T) {
	a := &sessionStatsEntry{}
	r := newStatsRing(sessionStatsWindowSec + 1)
	first := smp(0, a, 0, 0, 0)
	first.conns, first.rate = 3, "50M"
	last := smp(time.Second, a, 0, 0, 0)
	last.conns, last.rate = 1, "5M"
	r.push(first)
	r.push(last)
	if ev := r.event(); ev.Conns != 1 || ev.Rate != "5M" {
		t.Errorf("conns = %d, rate = %q; want 1, 5M", ev.Conns, ev.Rate)
	}
}

// The wire format web-ui is built against: rate and throttled are left out
// when there is nothing to say, and throttled 0 is not the same as absent.
// active is always there, false included: web-ui tells an older proxy,
// which never sends it, by its absence.
func TestSessionStatsEventJSON(t *testing.T) {
	cases := []struct {
		ev   sessionStatsEvent
		want string
	}{
		{sessionStatsEvent{WindowSec: 5}, `{"window_sec":5,"bytes_per_sec":0,"conns":0,"active":false}`},
		{sessionStatsEvent{WindowSec: 5, BytesPerSec: 655360.5, Conns: 2, Active: true, Rate: "5M", Throttled: ptr(0)},
			`{"window_sec":5,"bytes_per_sec":655360.5,"conns":2,"active":true,"rate":"5M","throttled":0}`},
		{sessionStatsEvent{WindowSec: 5, Active: true, Throttled: ptr(0.75)}, `{"window_sec":5,"bytes_per_sec":0,"conns":0,"active":true,"throttled":0.75}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.ev)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("got %s, want %s", b, c.want)
		}
	}
}

func TestSessionStatsAcquireRelease(t *testing.T) {
	s, clk := newTestSessionStats(t)
	k := statsKey("s1", "08ada5a7a6183aae1e09d831df6748d566095a10")
	limited := s.acquire(k, "5M", true)
	clk.Advance(time.Second)
	unlimited := s.acquire(k, "", false)
	if limited == nil || limited != unlimited {
		t.Fatalf("acquire gave %p and %p, want one entry for the key", limited, unlimited)
	}
	clk.Advance(2 * time.Second)
	x := s.sample(k)
	if x.entry != limited || x.conns != 2 {
		t.Fatalf("sample entry %p conns %d, want %p and 2", x.entry, x.conns, limited)
	}
	if x.rate != "" {
		t.Errorf("rate = %q: the most recent request had none, want empty", x.rate)
	}
	if x.limitedOpen != int64(3*time.Second) {
		t.Errorf("limited open = %v, want 3s: only the limited request counts", time.Duration(x.limitedOpen))
	}
	s.release(limited, true)
	clk.Advance(4 * time.Second)
	x = s.sample(k)
	if x.conns != 1 || x.limitedOpen != int64(3*time.Second) {
		t.Errorf("after the limited request ended: conns %d, limited open %v; want 1, 3s", x.conns, time.Duration(x.limitedOpen))
	}
	s.release(unlimited, false)
	if x = s.sample(k); x.conns != 0 {
		t.Errorf("conns = %d after both ended, want 0", x.conns)
	}
	if x.at != int64(7*time.Second) {
		t.Errorf("sample at %v from start, want 7s", time.Duration(x.at))
	}
}

// The limited open-time integral grows while any limited request is open,
// by one per request: two side by side count twice. The stream reads only
// whether it grew in the window (was a limited request open at all); the
// share is limiter wait over wall time.
func TestSessionStatsLimitedOpenSumsConnections(t *testing.T) {
	s, clk := newTestSessionStats(t)
	k := statsKey("s1", "h")
	e1 := s.acquire(k, "5M", true)
	clk.Advance(time.Second)
	e2 := s.acquire(k, "5M", true)
	clk.Advance(2 * time.Second)
	s.release(e1, true)
	clk.Advance(time.Second)
	s.release(e2, true)
	clk.Advance(time.Second)
	if got := s.sample(k).limitedOpen; got != int64(6*time.Second) {
		t.Errorf("limited open = %v, want 3s + 3s", time.Duration(got))
	}
}

func TestSessionStatsSampleWithoutEntry(t *testing.T) {
	s, clk := newTestSessionStats(t)
	clk.Advance(3 * time.Second)
	x := s.sample(statsKey("nobody", "h"))
	if x.entry != nil || x.statsTotals != (statsTotals{}) || x.conns != 0 || x.rate != "" {
		t.Errorf("sample of an unknown key = %+v, want zeros", x)
	}
	if x.at != int64(3*time.Second) {
		t.Errorf("sample at %v, want 3s", time.Duration(x.at))
	}
}

// An entry lives while it has open requests, and for 60 s after the last one.
func TestSessionStatsSweep(t *testing.T) {
	s, clk := newTestSessionStats(t)
	idle := statsKey("idle", "h")
	busy := statsKey("busy", "h")
	// The gauge is process-global and survives -count: compare deltas.
	gauge := gaugeValue(t, promSessionStatsEntries)
	e := s.acquire(idle, "", false)
	s.acquire(busy, "", false)
	if got := gaugeValue(t, promSessionStatsEntries) - gauge; got != 2 {
		t.Errorf("entries gauge grew by %v for two keys, want 2", got)
	}
	// Idle time counts from when the last request ended, not from creation.
	clk.Advance(30 * time.Second)
	s.release(e, false)
	clk.Advance(sessionStatsIdleTTL)
	s.sweep()
	if s.lookup(idle) == nil {
		t.Fatalf("entry idle for exactly %v was dropped, want kept", sessionStatsIdleTTL)
	}
	clk.Advance(time.Nanosecond)
	s.sweep()
	if s.lookup(idle) != nil {
		t.Errorf("entry idle for over %v was kept, want dropped", sessionStatsIdleTTL)
	}
	if got := gaugeValue(t, promSessionStatsEntries) - gauge; got != 1 {
		t.Errorf("entries gauge is %v over the start after one of two was dropped, want 1", got)
	}
	clk.Advance(10 * time.Minute)
	s.sweep()
	if s.lookup(busy) == nil {
		t.Errorf("entry with an open request was dropped")
	}
}

// The janitor sweeps on its own ticker and stops with Close.
func TestSessionStatsJanitor(t *testing.T) {
	clk := newFakeClock()
	s := newSessionStats(clk)
	k := statsKey("s", "h")
	s.release(s.acquire(k, "", false), false)
	if n := clk.liveTickers(sessionStatsSweepEvery); n != 1 {
		t.Fatalf("%d janitor tickers, want 1", n)
	}
	for i := 0; i < 7; i++ {
		clk.Advance(sessionStatsSweepEvery)
	}
	eventually(t, "the janitor to drop the idle entry", func() bool { return s.lookup(k) == nil })
	s.Close()
	if n := clk.liveTickers(sessionStatsSweepEvery); n != 0 {
		t.Errorf("%d janitor tickers still live after Close, want 0", n)
	}
	s.Close() // idempotent: Web.Close may run more than once
}

// A flood of sessions cannot grow the map without bound: when full, new keys
// go unaccounted and are counted; keys already there keep working.
func TestSessionStatsBound(t *testing.T) {
	s, clk := newTestSessionStats(t)
	s.maxEntries = 2
	k1, k2, k3 := statsKey("s1", "h"), statsKey("s2", "h"), statsKey("s3", "h")
	gauge := gaugeValue(t, promSessionStatsEntries)
	e1 := s.acquire(k1, "", false)
	s.acquire(k2, "", false)
	dropped := counterValue(t, promSessionStatsEntriesDropped.WithLabelValues("global"))
	if e := s.acquire(k3, "", false); e != nil {
		t.Fatalf("a third key got an entry in a map bounded at 2")
	}
	if got := counterValue(t, promSessionStatsEntriesDropped.WithLabelValues("global")) - dropped; got != 1 {
		t.Errorf("dropped counter grew by %v, want 1", got)
	}
	if got := gaugeValue(t, promSessionStatsEntries) - gauge; got != 2 {
		t.Errorf("entries gauge grew by %v, want 2: the refused key holds none", got)
	}
	if e := s.acquire(k1, "", false); e != e1 {
		t.Errorf("an existing key lost its entry while the map was full")
	}
	s.release(e1, false)
	s.release(e1, false)
	clk.Advance(sessionStatsIdleTTL + time.Second)
	s.sweep()
	if e := s.acquire(k3, "", false); e == nil {
		t.Errorf("no room for a new key after an idle one was dropped")
	}
}

// One token is good for any infohash: a session gets its share of the map
// and no more, so it cannot leave every other viewer on the pod uncounted.
func TestSessionStatsPerSessionBound(t *testing.T) {
	s, clk := newTestSessionStats(t)
	s.maxEntriesPerSession = 2
	a1, a2, a3 := statsKey("a", "h1"), statsKey("a", "h2"), statsKey("a", "h3")
	dropped := counterValue(t, promSessionStatsEntriesDropped.WithLabelValues("session"))
	e1 := s.acquire(a1, "", false)
	s.acquire(a2, "", false)
	if e := s.acquire(a3, "", false); e != nil {
		t.Fatalf("a session got a third key with a share of 2")
	}
	if got := counterValue(t, promSessionStatsEntriesDropped.WithLabelValues("session")) - dropped; got != 1 {
		t.Errorf("dropped{session} grew by %v, want 1", got)
	}
	if e := s.acquire(a1, "", false); e != e1 {
		t.Errorf("a key the session holds lost its entry at the share")
	}
	if s.acquire(statsKey("b", "h3"), "", false) == nil {
		t.Errorf("another session was refused over a's share")
	}
	// The same sessionID under another domain (an embed's visitors) is a
	// session of its own.
	if s.acquire(sessionStatsKey{sessionID: "a", domain: "embed.example", infoHash: "h3"}, "", false) == nil {
		t.Errorf("a's sessionID under another domain was refused over a's share")
	}
	s.release(e1, false)
	s.release(e1, false)
	clk.Advance(sessionStatsIdleTTL + time.Second)
	s.sweep()
	if s.acquire(a3, "", false) == nil {
		t.Errorf("no room in a's share after one of its keys was dropped")
	}
}

// The map keeps a key for a minute after its request; the key's strings
// must be its own, not slices of whatever the caller built them from (the
// infohash is a slice of the request line, which can run to megabytes).
func TestSessionStatsAcquireCopiesKey(t *testing.T) {
	s, _ := newTestSessionStats(t)
	line := "s1 embed.example " + harnessHash + strings.Repeat("x", 1<<10)
	k := sessionStatsKey{sessionID: line[:2], domain: line[3:16], infoHash: line[17:57]}
	if k.infoHash != harnessHash || k.domain != "embed.example" {
		t.Fatalf("bad slicing: %+v", k)
	}
	s.acquire(k, "", false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) != 1 || len(s.perSession) != 1 {
		t.Fatalf("%d entries, %d sessions; want 1, 1", len(s.entries), len(s.perSession))
	}
	for held := range s.entries {
		for name, v := range map[string]string{"sessionID": held.sessionID, "domain": held.domain, "infoHash": held.infoHash} {
			if aliases(v, line) {
				t.Errorf("the map's key %s points into the caller's string", name)
			}
		}
	}
	for held := range s.perSession {
		if aliases(held.sessionID, line) || aliases(held.domain, line) {
			t.Errorf("the per-session count's key points into the caller's string")
		}
	}
}

// discardResponse is a ResponseWriter that drops the body.
type discardResponse struct{ h http.Header }

func (d discardResponse) Header() http.Header         { return d.h }
func (d discardResponse) Write(p []byte) (int, error) { return len(p), nil }
func (d discardResponse) WriteHeader(int)             {}

// Every Write of every counted response goes through the writer: after the
// entry is resolved it may not allocate. Locks and map lookups do not
// allocate; TestSessionStatsWriterWriteTakesNoLock covers those.
func TestSessionStatsWriterWriteDoesNotAllocate(t *testing.T) {
	s, _ := newTestSessionStats(t)
	tw := NewThrottledRequestWrtier(discardResponse{http.Header{}}, &perByteThrottler{})
	w := &sessionStatsWriter{ResponseWriter: tw, stats: s, key: statsKey("s", "h"), rate: "5M", tw: tw}
	w.WriteHeader(http.StatusOK)
	p := make([]byte, 32<<10)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = w.Write(p) }); allocs != 0 {
		t.Errorf("Write allocates %v times, want 0", allocs)
	}
	if got := s.lookup(w.key).bytes.Load(); got != 101*32<<10 {
		t.Errorf("entry holds %d bytes, want %d", got, 101*32<<10)
	}
}

// Nor may a Write wait on a lock: every open stream samples under them once
// a second, and the janitor sweeps the whole map under s.mu. With all of
// them held, Write still returns.
func TestSessionStatsWriterWriteTakesNoLock(t *testing.T) {
	s, _ := newTestSessionStats(t)
	tw := NewThrottledRequestWrtier(discardResponse{http.Header{}}, &perByteThrottler{})
	w := &sessionStatsWriter{ResponseWriter: tw, stats: s, key: statsKey("s", "h"), rate: "5M", tw: tw}
	w.WriteHeader(http.StatusOK)
	e := s.lookup(w.key)
	if e == nil {
		t.Fatal("no entry after WriteHeader(200)")
	}
	s.mu.Lock()
	s.streamsMu.Lock()
	e.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.Write(make([]byte, 1000))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Write blocked on a SessionStats lock")
	}
	e.mu.Unlock()
	s.streamsMu.Unlock()
	s.mu.Unlock()
	<-done
	if got := e.bytes.Load(); got != 1000 {
		t.Errorf("entry holds %d bytes, want 1000", got)
	}
}
