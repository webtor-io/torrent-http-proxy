package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// countingThrottler counts Wait calls and never blocks.
type countingThrottler struct{ calls atomic.Int64 }

func (c *countingThrottler) Wait(int64) { c.calls.Add(1) }

// subrequest is nginx-vod's read of the mp4 behind a viewer's segment: the
// viewer's token, and X-Forwarded-For and X-Real-IP as nginx passes them on,
// here naming the internal pod itself, which is exactly what a client that
// wants to pass for one would write. Only peer differs between callers.
func (h *throttleHarness) subrequest(t *testing.T, claims jwt.MapClaims, peer string) *http.Request {
	t.Helper()
	r := h.request(t, claims, true, "")
	r.Header.Set("X-Forwarded-For", harnessCallerIP)
	r.Header.Set("X-Real-IP", harnessCallerIP)
	r.Header.Set("X-Request-ID", "req-vod")
	r.RemoteAddr = peer
	return r
}

// serveRaw is serve for a request that may be refused before the closing
// line: it returns the recorder, the closing line's fields (nil when there
// was none) and the Source proxyHTTP routed.
func (h *throttleHarness) serveRaw(t *testing.T, r *http.Request) (*httptest.ResponseRecorder, logrus.Fields, *Source) {
	t.Helper()
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	rec := httptest.NewRecorder()
	logger, hook := logtest.NewNullLogger()
	h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))
	for _, e := range hook.AllEntries() {
		if closingMessages[e.Message] {
			return rec, e.Data, src
		}
	}
	return rec, nil, src
}

func sessionTotal(l *SessionLimiter, sid string) int32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.sessions[sid]; ok {
		return s.total.Load()
	}
	return 0
}

// The owner's rule (2026-09-27): limiters apply to external connections
// only. A pod of a routed service fetching on the viewer's behalf with the
// viewer's token takes no session limiter slot, does not wait on the
// session's bucket and is not the viewer's traffic in session stats. The
// same request, headers and all, from any other peer is limited as before.
func TestWebInternalCallerIsNotLimitedOrAccounted(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		peer     string
		internal bool
	}{
		{"pod of a routed service", "t-caller-int", harnessCallerIP + ":41234", true},
		{"any other peer, same headers", "t-caller-ext", "192.0.2.1:1234", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sid := "s-" + c.role
			claims := tierClaims(c.role, sid, "2M")
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1<<20))
			th := &countingThrottler{}
			h.useThrottler(t, claims, th)
			h.web.sl = &SessionLimiter{maxTotal: 1, sessions: map[string]*sessionState{}}
			// The viewer's own segment request holds the session's one slot.
			release, reason := h.web.sl.Acquire(sid, harnessHash, "/Sintel/Sintel.mkv", "203.0.113.7")
			if release == nil {
				t.Fatalf("the viewer's own request was refused (%s): the case proves nothing", reason)
			}

			rec, f, src := h.serveRaw(t, h.subrequest(t, claims, c.peer))
			if src.Internal {
				t.Errorf("routed as internal with X-Forwarded-For present: routing must not follow the caller")
			}
			if !c.internal {
				if rec.Code != http.StatusTooManyRequests {
					t.Fatalf("at the session's cap: %d, want 429", rec.Code)
				}
				release()
				rec, f, _ = h.serveRaw(t, h.subrequest(t, claims, c.peer))
				if rec.Code != http.StatusOK || rec.Body.Len() != 1<<20 {
					t.Fatalf("slot free: %d with %d bytes, want 200 with %d", rec.Code, rec.Body.Len(), 1<<20)
				}
				if th.calls.Load() == 0 {
					t.Error("the session's bucket was never waited on")
				}
				seconds(t, f, "throttled")
				if f["source"] != string(External) {
					t.Errorf("source = %v, want %s", f["source"], External)
				}
				if v, ok := f["caller"]; ok {
					t.Errorf("caller = %v on an external request, want absent", v)
				}
				if n := entryCount(h.web.stats); n != 1 {
					t.Errorf("%d session stats entries, want the session's 1", n)
				}
				expectSourceSeries(t, c.role, External)
				return
			}
			defer release()
			if rec.Code != http.StatusOK || rec.Body.Len() != 1<<20 {
				t.Fatalf("session at its cap: %d with %d bytes, want 200 with %d", rec.Code, rec.Body.Len(), 1<<20)
			}
			if n := sessionTotal(h.web.sl, sid); n != 1 {
				t.Errorf("session holds %d slots after the internal request, want the viewer's 1", n)
			}
			if n := th.calls.Load(); n != 0 {
				t.Errorf("the session's bucket was waited on %d times, want 0", n)
			}
			if v, ok := f["throttled"]; ok {
				t.Errorf("throttled = %v, want absent: no limiter", v)
			}
			if f["source"] != string(Internal) || f["caller"] != harnessCallerSvc {
				t.Errorf("source = %v, caller = %v; want %s, %s", f["source"], f["caller"], Internal, harnessCallerSvc)
			}
			if n := entryCount(h.web.stats); n != 0 {
				t.Errorf("%d session stats entries, want none", n)
			}
			if f["req_id"] != "req-vod" {
				t.Errorf("req_id = %v, want the one the subrequest carried", f["req_id"])
			}
			expectSourceSeries(t, c.role, Internal)
		})
	}
}

// expectSourceSeries checks every request_total series of role is source.
func expectSourceSeries(t *testing.T, role string, source SourceType) {
	t.Helper()
	series := roleSeries(t, promHTTPProxyRequestTotal, role)
	if len(series) == 0 {
		t.Fatalf("no request_total series for %s", role)
	}
	for _, m := range series {
		if got := metricLabel(m, "source"); got != string(source) {
			t.Errorf("request_total{role=%s} has source=%s, want %s", role, got, source)
		}
	}
}

// Routing keeps its own answer (X-Forwarded-For, for preferLocal); the
// caller comes from the peer alone. An in-cluster peer that is not a pod of
// a routed service is external for limits whatever it sends.
func TestWebRoutingIsNotTheCaller(t *testing.T) {
	cases := []struct {
		name         string
		peer         string
		xff          bool
		wantRouteInt bool
		wantSource   SourceType
	}{
		{"routed pod, forwarded (nginx-vod)", harnessCallerIP + ":1", true, false, Internal},
		{"routed pod, not forwarded (FFmpeg)", harnessCallerIP + ":1", false, true, Internal},
		{"unknown peer, not forwarded", "10.0.99.1:1", false, true, External},
		{"unknown peer, forwarded (ingress)", "10.0.68.139:1", true, false, External},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 10))
			r := h.request(t, jwt.MapClaims{"role": "t-route"}, c.xff, "")
			r.RemoteAddr = c.peer
			rec, f, src := h.serveRaw(t, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			if src.Internal != c.wantRouteInt {
				t.Errorf("Source.Internal = %v, want %v", src.Internal, c.wantRouteInt)
			}
			if f["source"] != string(c.wantSource) {
				t.Errorf("source = %v, want %s", f["source"], c.wantSource)
			}
		})
	}
}

// Over a real connection: the peer is what net/http took from the accepted
// socket, loopback here. Listed, the request is internal whatever its
// headers say; not listed, X-Forwarded-For and X-Real-IP naming a listed pod
// do not make it one.
func TestWebCallerIsTheConnectionPeer(t *testing.T) {
	for _, listed := range []bool{true, false} {
		name := "loopback listed"
		if !listed {
			name = "loopback not listed, headers name a listed pod"
		}
		t.Run(name, func(t *testing.T) {
			hook := captureLog(t)
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
			srv := newStatsServer(t, h.web)
			h.web.callers.addrs.Store(loopbackCallers(listed))
			role := "t-peer-" + map[bool]string{true: "listed", false: "unlisted"}[listed]
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/"+harnessHash+"/Sintel/Sintel.mkv?token="+
				signToken(t, h.secret, tierClaims(role, "s-"+role, "2M"))+"&api-key=k", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Forwarded-For", harnessCallerIP)
			req.Header.Set("X-Real-IP", harnessCallerIP)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || len(body) != 1000 {
				t.Fatalf("%d with %d bytes, want 200 with 1000", resp.StatusCode, len(body))
			}
			want, entries := External, 1
			if listed {
				want, entries = Internal, 0
			}
			var f logrus.Fields
			eventually(t, "the closing log line", func() bool {
				for _, e := range hook.AllEntries() {
					if closingMessages[e.Message] && e.Data["role"] == role {
						f = e.Data
						return true
					}
				}
				return false
			})
			if f["source"] != string(want) {
				t.Errorf("source = %v, want %s", f["source"], want)
			}
			if _, throttled := f["throttled"]; throttled == listed {
				t.Errorf("throttled field present = %v, want %v", throttled, !listed)
			}
			if n := entryCount(h.web.stats); n != entries {
				t.Errorf("%d session stats entries, want %d", n, entries)
			}
		})
	}
}
