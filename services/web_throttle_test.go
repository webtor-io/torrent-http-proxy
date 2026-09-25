package services

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/dgrijalva/jwt-go"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/lazymap"
)

// throttleTestSvc is the "default" service of the harness; its name is also
// the `name` label the metrics carry.
const throttleTestSvc = "thp-throttle-test"

// throttleHarness runs proxyHTTP end to end: token check, bucket lookup,
// resolver, a real ReverseProxy (FlushInterval -1, as in prod) and an
// upstream the case supplies. The pool is local-only; a case that asserts
// on timing injects its own Throttler rather than lean on the real bucket.
type throttleHarness struct {
	web    *Web
	secret string
	// clock drives the session stats: tests move it by hand.
	clock *fakeClock
}

func newThrottleHarness(t *testing.T, upstream http.HandlerFunc) *throttleHarness {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("THP_THROTTLE_TEST_SERVICE_HOST", u.Hostname())
	t.Setenv("THP_THROTTLE_TEST_SERVICE_PORT", u.Port())
	cfg := &ServicesConfig{"default": {Name: throttleTestSvc, EndpointsProvider: Environment}}
	res := NewResolver(cfg, &ServiceLocation{
		LazyMap: lazymap.New[*Location](&lazymap.Config{Expire: time.Minute}),
	})
	pr := &HTTPProxy{
		r:                 res,
		transport:         &http.Transport{},
		externalTransport: &http.Transport{},
		LazyMap:           lazymap.New[*httputil.ReverseProxy](&lazymap.Config{Expire: time.Minute}),
	}
	secret := "throttle-test-secret"
	clk := newFakeClock()
	stats := newSessionStats(clk)
	t.Cleanup(stats.Close)
	return &throttleHarness{
		secret: secret,
		clock:  clk,
		web: &Web{
			baseURL:        "http://thp.test",
			parser:         NewURLParser(cfg),
			r:              res,
			pr:             pr,
			claims:         &Claims{apiKey: "k", apiSecret: secret},
			bucket:         NewHybridBucketPool(nil),
			bandwidthLimit: true,
			stats:          stats,
			closing:        make(chan struct{}),
			gs:             cs.NewGracefulServer(time.Second),
		},
	}
}

// fixedBody answers with status and size zero bytes in one Write.
func fixedBody(status, size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(status)
		_, _ = w.Write(make([]byte, size))
	}
}

// request builds a request for the harness's torrent. External ones carry
// X-Forwarded-For, as everything that comes through the ingress does.
func (h *throttleHarness) request(t *testing.T, claims jwt.MapClaims, external bool, query string) *http.Request {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.secret))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet,
		"/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel/Sintel.mkv?token="+tok+"&api-key=k"+query, nil)
	if external {
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
	}
	return r
}

// serve sends r through proxyHTTP into w and returns the fields of the
// closing "served / bad / failed" log line.
func (h *throttleHarness) serve(t *testing.T, w http.ResponseWriter, r *http.Request) logrus.Fields {
	t.Helper()
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path // what Serve does for a mod-less source
	logger, hook := logtest.NewNullLogger()
	h.web.proxyHTTP(w, r, src, logrus.NewEntry(logger))
	for _, e := range hook.AllEntries() {
		switch e.Message {
		case "request served successfully", "bad request", "failed to serve request":
			return e.Data
		}
	}
	t.Fatalf("no closing log line among %d entries", len(hook.AllEntries()))
	return nil
}

// perByteThrottler blocks Wait(n) for n*perByte: a limiter whose bucket is
// always dry. The zero value is a limiter that never binds.
type perByteThrottler struct{ perByte time.Duration }

func (p *perByteThrottler) Wait(n int64) { time.Sleep(time.Duration(n) * p.perByte) }

// useThrottler makes th the limiter proxyHTTP installs for claims' session,
// so the case decides how long Wait blocks.
func (h *throttleHarness) useThrottler(t *testing.T, claims jwt.MapClaims, th Throttler) {
	t.Helper()
	sid, _ := claims["sessionID"].(string)
	rate, _ := claims["rate"].(string)
	// HybridBucketPool keys a session's bucket by sessionID+rate.
	if _, err := h.web.bucket.LazyMap.Get(sid+rate, func() (Throttler, error) { return th, nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := h.web.bucket.Get(claims); err != nil || got != th {
		t.Fatalf("pool hands out %v (%v), not the injected throttler", got, err)
	}
}

// tierClaims is a user token with a limiter: "2M" is 256 KiB/s, whose four
// seconds are exactly the 1 MiB floor, so a 1 MiB response is observed.
func tierClaims(role, session, rate string) jwt.MapClaims {
	return jwt.MapClaims{"role": role, "sessionID": session, "rate": rate}
}

// roleSeries returns the series of c carrying role, read through Collect.
// WithLabelValues would create the series it reads, and "no series at all"
// is exactly what a request without a limiter must leave behind.
func roleSeries(t *testing.T, c prometheus.Collector, role string) []*dto.Metric {
	t.Helper()
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	var out []*dto.Metric
	for m := range ch {
		d := &dto.Metric{}
		if err := m.Write(d); err != nil {
			t.Fatal(err)
		}
		if metricLabel(d, "role") == role {
			out = append(out, d)
		}
	}
	return out
}

func metricLabel(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// throttleSnap is what the throttle metrics hold for one role. The vectors
// are process-global and survive go test -count=N, so a case compares the
// snapshots taken before and after its request, never absolute values.
type throttleSnap struct {
	series                     int // over all four vectors
	wait, duration, downstream float64
	ratioCount                 map[string]uint64  // by download label
	ratioSum                   map[string]float64 // by download label
}

func snapThrottle(t *testing.T, role string) throttleSnap {
	t.Helper()
	s := throttleSnap{ratioCount: map[string]uint64{}, ratioSum: map[string]float64{}}
	counters := []struct {
		c   prometheus.Collector
		sum *float64
	}{
		{promHTTPProxyThrottleWait, &s.wait},
		{promHTTPProxyThrottledDuration, &s.duration},
		{promHTTPProxyThrottledDownstream, &s.downstream},
	}
	for _, c := range counters {
		for _, m := range roleSeries(t, c.c, role) {
			s.series++
			*c.sum += m.GetCounter().GetValue()
		}
	}
	for _, m := range roleSeries(t, promHTTPProxyThrottleRatio, role) {
		s.series++
		d := metricLabel(m, "download")
		s.ratioCount[d] += m.GetHistogram().GetSampleCount()
		s.ratioSum[d] += m.GetHistogram().GetSampleSum()
	}
	return s
}

// seconds returns a float log field, failing the case when it is missing.
func seconds(t *testing.T, f logrus.Fields, key string) float64 {
	t.Helper()
	v, ok := f[key].(float64)
	if !ok {
		t.Fatalf("%s field = %#v, want seconds", key, f[key])
	}
	return v
}

// sameSeconds compares a counter delta with the log field it must equal:
// both are the one value the closing closure computed.
func sameSeconds(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// checkCountersMatchLog asserts that the counter deltas are the log line's
// own throttled / duration / downstream_blocked values, and that a
// response's parts do not exceed its duration.
func checkCountersMatchLog(t *testing.T, before, after throttleSnap, f logrus.Fields) {
	t.Helper()
	throttled := seconds(t, f, "throttled")
	duration := seconds(t, f, "duration")
	downstream := seconds(t, f, "downstream_blocked")
	if got := after.wait - before.wait; !sameSeconds(got, throttled) {
		t.Errorf("wait counter grew by %v, log says throttled=%v", got, throttled)
	}
	if got := after.duration - before.duration; !sameSeconds(got, duration) {
		t.Errorf("duration counter grew by %v, log says duration=%v", got, duration)
	}
	if got := after.downstream - before.downstream; !sameSeconds(got, downstream) {
		t.Errorf("downstream counter grew by %v, log says downstream_blocked=%v", got, downstream)
	}
	if throttled+downstream > duration {
		t.Errorf("throttled %v + downstream_blocked %v exceed duration %v", throttled, downstream, duration)
	}
}

// Limiter holds every byte (200 ns/byte, ~0.2 s per MiB), upstream answers
// at once: tier-bound, so nearly all of the duration is limiter wait.
func TestThrottleMetricsTierBound(t *testing.T) {
	const role = "t-tier"
	claims := tierClaims(role, "s-tier", "2M")
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
	h.useThrottler(t, claims, &perByteThrottler{200 * time.Nanosecond})
	r := h.request(t, claims, true, "&download=true")
	r.Header.Set("X-Request-ID", "req-tier")
	rec := httptest.NewRecorder()
	before := snapThrottle(t, role)
	f := h.serve(t, rec, r)
	after := snapThrottle(t, role)
	if rec.Code != http.StatusOK || rec.Body.Len() != 1<<20 {
		t.Fatalf("got %d with %d bytes, want 200 with %d", rec.Code, rec.Body.Len(), 1<<20)
	}
	checkCountersMatchLog(t, before, after, f)
	if n := after.ratioCount["true"] - before.ratioCount["true"]; n != 1 {
		t.Errorf("ratio observations with download=true grew by %d, want 1", n)
	}
	if n := after.ratioCount["false"] - before.ratioCount["false"]; n != 0 {
		t.Errorf("ratio observations with download=false grew by %d, want 0", n)
	}
	if got := after.ratioSum["true"] - before.ratioSum["true"]; got < 0.8 || got > 1 {
		t.Errorf("observed ratio %v, want a tier-bound share in [0.8, 1]", got)
	}
	waits := roleSeries(t, promHTTPProxyThrottleWait, role)
	if len(waits) != 1 || metricLabel(waits[0], "source") != string(External) || metricLabel(waits[0], "name") != throttleTestSvc {
		t.Errorf("wait series for %s = %v, want one {source=%s, name=%s}", role, waits, External, throttleTestSvc)
	}
	if f["bytes"] != 1<<20 {
		t.Errorf("bytes field = %v, want %d", f["bytes"], 1<<20)
	}
	if f["req_id"] != "req-tier" {
		t.Errorf("req_id field = %#v, want the ingress X-Request-ID", f["req_id"])
	}
}

// Limiter never binds, upstream trickles 16 chunks 20 ms apart: slow for the
// upstream's sake, so the ratio stays near 0.
func TestThrottleMetricsUpstreamBound(t *testing.T) {
	const role = "t-upstream"
	claims := tierClaims(role, "s-upstream", "2M")
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 16; i++ {
			_, _ = w.Write(make([]byte, 64<<10))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	h.useThrottler(t, claims, &perByteThrottler{})
	before := snapThrottle(t, role)
	f := h.serve(t, httptest.NewRecorder(), h.request(t, claims, true, ""))
	after := snapThrottle(t, role)
	checkCountersMatchLog(t, before, after, f)
	if n := after.ratioCount["false"] - before.ratioCount["false"]; n != 1 {
		t.Fatalf("ratio observations with download=false grew by %d, want 1", n)
	}
	if got := after.ratioSum["false"] - before.ratioSum["false"]; got > 0.1 {
		t.Errorf("observed ratio %v, want an upstream-bound share under 0.1", got)
	}
}

// Limiter and upstream are instant, the reader takes 5 ms for every Write and
// every Flush: that time is the downstream's and is recorded as such, not
// left in the remainder that reads as "upstream".
func TestThrottleMetricsDownstreamBound(t *testing.T) {
	const role = "t-downstream"
	claims := tierClaims(role, "s-downstream", "2M")
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
	h.useThrottler(t, claims, &perByteThrottler{})
	before := snapThrottle(t, role)
	f := h.serve(t, blockingDownstream{httptest.NewRecorder(), 5 * time.Millisecond, 5 * time.Millisecond},
		h.request(t, claims, true, ""))
	after := snapThrottle(t, role)
	checkCountersMatchLog(t, before, after, f)
	// ReverseProxy copies at most 32 KiB per Write and flushes after each,
	// so 1 MiB is at least 32 Writes and 32 Flushes of 5 ms.
	if got := seconds(t, f, "downstream_blocked"); got < 0.32 {
		t.Errorf("downstream_blocked = %v, want at least 0.32 s", got)
	}
	if got, d := seconds(t, f, "throttled"), seconds(t, f, "duration"); got > 0.05*d {
		t.Errorf("throttled = %v of %v: downstream time leaked into limiter wait", got, d)
	}
}

// The seeder serves an attachment whenever the download key is present, and
// the label follows it; rest-api spells it download=true.
func TestThrottleMetricsDownloadLabel(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"download=true", "&download=true", "true"},
		{"bare key", "&download", "true"},
		{"download=1", "&download=1", "true"},
		{"no key", "", "false"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role := "t-dl-" + strconv.Itoa(i)
			claims := tierClaims(role, "s-dl-"+strconv.Itoa(i), "2M")
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
			h.useThrottler(t, claims, &perByteThrottler{})
			before := snapThrottle(t, role)
			h.serve(t, httptest.NewRecorder(), h.request(t, claims, true, c.query))
			after := snapThrottle(t, role)
			for _, l := range []string{"true", "false"} {
				want := uint64(0)
				if l == c.want {
					want = 1
				}
				if n := after.ratioCount[l] - before.ratioCount[l]; n != want {
					t.Errorf("ratio observations with download=%s grew by %d, want %d", l, n, want)
				}
			}
		})
	}
}

// Status streams (the seeder's ?stats=true, warmup) hold a limited
// connection for minutes and barely write: they are logged, but stay out of
// every throttle metric so they do not dilute the content-throughput shares.
func TestThrottleMetricsSkipEventStreams(t *testing.T) {
	const role = "t-sse"
	claims := tierClaims(role, "s-sse", "2M")
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1<<20))
	})
	h.useThrottler(t, claims, &perByteThrottler{})
	f := h.serve(t, httptest.NewRecorder(), h.request(t, claims, true, "&stats=true"))
	if _, ok := f["throttled"]; !ok {
		t.Fatalf("throttled field missing: the limiter was not installed, the case proves nothing")
	}
	if n := snapThrottle(t, role).series; n != 0 {
		t.Errorf("event stream left %d throttle series, want none", n)
	}
}

// A limiter was installed, so the counters record the 404, but a response
// that is not content says nothing about throughput: no ratio.
func TestThrottleMetricsErrorHasNoRatio(t *testing.T) {
	const role = "t-notfound"
	claims := tierClaims(role, "s-notfound", "2M")
	h := newThrottleHarness(t, fixedBody(http.StatusNotFound, 1<<20))
	h.useThrottler(t, claims, &perByteThrottler{})
	before := snapThrottle(t, role)
	rec := httptest.NewRecorder()
	f := h.serve(t, rec, h.request(t, claims, true, ""))
	after := snapThrottle(t, role)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	checkCountersMatchLog(t, before, after, f)
	if n := after.ratioCount["false"] - before.ratioCount["false"]; n != 0 {
		t.Errorf("ratio observations for a 404 grew by %d, want 0", n)
	}
}

// At "1M" (128 KiB/s) four seconds of rate is only 512 KiB; the 1 MiB floor
// still keeps a 768 KiB response out of the ratio.
func TestThrottleMetricsRatioByteFloor(t *testing.T) {
	const role = "t-floor"
	claims := tierClaims(role, "s-floor", "1M")
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 768<<10))
	h.useThrottler(t, claims, &perByteThrottler{})
	before := snapThrottle(t, role)
	h.serve(t, httptest.NewRecorder(), h.request(t, claims, true, ""))
	after := snapThrottle(t, role)
	if after.wait == before.wait && after.duration == before.duration {
		t.Fatalf("counters did not record the response: the limiter was not installed, the case proves nothing")
	}
	if n := after.ratioCount["false"] - before.ratioCount["false"]; n != 0 {
		t.Errorf("ratio observations for %d bytes grew by %d, want 0", 768<<10, n)
	}
}

// The prod bucket is Redis-backed, and a fresh (or 1 s idle) key starts full:
// the first `rate` bytes of a session leave without waiting, the rest are
// paced. At "40M" (5 MiB/s) a 4 MiB response is all burst and a 7 MiB one
// waits ~0.4 s, yet both are under four seconds of rate (20 MiB), so neither
// enters the ratio: burst-served responses read ≈ 0 whatever the upstream did.
func TestThrottleMetricsRatioSkipsBurstOnRedis(t *testing.T) {
	const role = "t-burst"
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	cases := []struct {
		size                 int
		minWaited, maxWaited float64
	}{
		{4 << 20, 0, 0.05},
		{7 << 20, 0.2, math.Inf(1)},
	}
	for i, c := range cases {
		h := newThrottleHarness(t, fixedBody(http.StatusOK, c.size))
		h.web.bucket = NewHybridBucketPool(rc)
		before := snapThrottle(t, role)
		f := h.serve(t, httptest.NewRecorder(), h.request(t, tierClaims(role, "s-burst-"+strconv.Itoa(i), "40M"), true, ""))
		after := snapThrottle(t, role)
		if got := seconds(t, f, "throttled"); got < c.minWaited || got > c.maxWaited {
			t.Errorf("%d bytes on a fresh 5 MiB/s key: throttled = %v, want in [%v, %v]", c.size, got, c.minWaited, c.maxWaited)
		}
		if n := after.ratioCount["false"] - before.ratioCount["false"]; n != 0 {
			t.Errorf("%d bytes at 40M: ratio observations grew by %d, want 0", c.size, n)
		}
	}
}

// No limiter is not "a limiter that never waited": no throttle series at all
// and no throttled field. Grace-segment tokens have a rate but no sessionID,
// so they land here too.
func TestThrottleMetricsWithoutLimiter(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		claims   jwt.MapClaims
		external bool
		limitOff bool
	}{
		{"internal request", "t-internal", tierClaims("t-internal", "s-internal", "2M"), false, false},
		{"token without rate", "t-norate", jwt.MapClaims{"role": "t-norate", "sessionID": "s-norate"}, true, false},
		{"token without session (grace)", "t-nosession", jwt.MapClaims{"role": "t-nosession", "rate": "2M"}, true, false},
		{"bandwidth limit off", "t-limitoff", tierClaims("t-limitoff", "s-limitoff", "2M"), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
			h.web.bandwidthLimit = !c.limitOff
			rec := httptest.NewRecorder()
			f := h.serve(t, rec, h.request(t, c.claims, c.external, ""))
			if rec.Body.Len() != 1<<20 {
				t.Fatalf("got %d bytes, want %d", rec.Body.Len(), 1<<20)
			}
			if n := snapThrottle(t, c.role).series; n != 0 {
				t.Errorf("request without a limiter left %d throttle series, want none", n)
			}
			if v, ok := f["throttled"]; ok {
				t.Errorf("throttled field = %v, want absent", v)
			}
			if f["bytes"] != 1<<20 {
				t.Errorf("bytes field = %v, want %d", f["bytes"], 1<<20)
			}
			seconds(t, f, "downstream_blocked")
			if v, ok := f["req_id"]; ok {
				t.Errorf("req_id field = %v without an X-Request-ID, want absent", v)
			}
		})
	}
}

func TestThrottleRatioIsAShare(t *testing.T) {
	cases := []struct {
		waited, total time.Duration
		want          float64
	}{
		{0, time.Second, 0},
		{250 * time.Millisecond, time.Second, 0.25},
		{time.Second, time.Second, 1},
		{2 * time.Second, time.Second, 1},
		{time.Second, 0, 0},
	}
	for _, c := range cases {
		if got := throttleRatio(c.waited, c.total); got != c.want {
			t.Errorf("throttleRatio(%v, %v) = %v, want %v", c.waited, c.total, got, c.want)
		}
	}
}

func TestThrottleRatioMinSize(t *testing.T) {
	cases := []struct {
		rate string
		want int
	}{
		{"1M", 1 << 20},   // 4 s of 128 KiB/s is 512 KiB: the byte floor wins
		{"2M", 1 << 20},   // 4 s of 256 KiB/s
		{"5M", 5 << 19},   // free: 4 s of 640 KiB/s
		{"50M", 25 << 20}, // 4 s of 6.25 MiB/s
		{"not-a-rate", math.MaxInt},
	}
	for _, c := range cases {
		if got := throttleRatioMinSize(c.rate); got != c.want {
			t.Errorf("throttleRatioMinSize(%q) = %d, want %d", c.rate, got, c.want)
		}
	}
}
