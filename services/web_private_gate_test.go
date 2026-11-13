package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
)

// gateRefusals are the gate's refusal lines among entries.
func gateRefusals(entries []*logrus.Entry) []*logrus.Entry {
	var out []*logrus.Entry
	for _, e := range entries {
		if e.Message == "private gate refused" {
			out = append(out, e)
		}
	}
	return out
}

// fieldIs: f has key with the value want, or has no key when want is "".
func fieldIs(f logrus.Fields, key, want string) bool {
	v, ok := f[key]
	if want == "" {
		return !ok
	}
	return v == want
}

// countingUpstream is a harness whose upstream answers 200 and counts the
// requests that reached it.
func countingUpstream(t *testing.T) (*throttleHarness, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	h := newThrottleHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	return h, &hits
}

// TestGateWiring: proxyHTTP asks the gate before anything reaches a service,
// on every edge that names a torrent, and not on /ext/. A refusal is one Warn
// without the token: 403 with the reason when the token has no right, the
// lookup's bare status when rest-api did not answer. Without a gate nothing
// changes. The vod edge is the harness's caller service on the same upstream,
// as in TestUpstreamUidFromVerifiedToken.
func TestGateWiring(t *testing.T) {
	h, upstream := countingUpstream(t)
	withVodEdge(t, h)
	rest := newFakeRestAPI(t, restAnswer(200, `{"private":true}`))
	h.web.gate = newPrivateGate(rest.URL, true, time.Second, time.Minute)
	bound, boundUid := jwt.MapClaims{"hash": harnessHash}, jwt.MapClaims{"hash": harnessHash, "uid": harnessUid}
	// base64 of https://example.org/sub.srt, as in TestHashBindingCoversTorrentsOnly.
	const ext = "/ext/aHR0cHM6Ly9leGFtcGxlLm9yZy9zdWIuc3J0/sub.srt"
	// serve sends claims' request for path ("" is the harness's file) and
	// returns its token and how many requests it brought the upstream.
	serve := func(path string, claims jwt.MapClaims) (*httptest.ResponseRecorder, []*logrus.Entry, string, int32) {
		r := h.request(t, claims, true, "")
		if path != "" {
			r.URL.Path = path
		}
		before := upstream.Load()
		rec, entries, _ := h.serveLogged(t, r)
		return rec, entries, r.URL.Query().Get("token"), upstream.Load() - before
	}
	// refused checks a refusal: one log line, the gate's Warn, with the hash,
	// outcome and cause, and the token nowhere.
	refused := func(name string, entries []*logrus.Entry, token, outcome, cause string) {
		t.Helper()
		if len(entries) != 1 || len(gateRefusals(entries)) != 1 {
			t.Fatalf("%s: %d entries, want the one refusal Warn", name, len(entries))
		}
		e := entries[0]
		if e.Level != logrus.WarnLevel || e.Data["infohash"] != harnessHash || e.Data["outcome"] != outcome || !fieldIs(e.Data, "cause", cause) {
			t.Errorf("%s: %s %q %v, want Warn with infohash, outcome %s, cause %q", name, e.Level, e.Message, e.Data, outcome, cause)
		}
		if _, ok := e.Data["token"]; ok || strings.Contains(fmt.Sprint(e.Data), token) || strings.Contains(e.Message, token) {
			t.Errorf("%s: the token is in the log: %v", name, e.Data)
		}
	}

	rec, entries, _, up := serve("", boundUid)
	if f := closingFields(entries); rec.Code != http.StatusOK || up != 1 || f["private_gate"] != gateAllowed || !fieldIs(f, "private_gate_cause", "") {
		t.Errorf("bound with uid: %d, upstream %d, closing line %v; want 200, 1, private_gate=allowed without a cause", rec.Code, up, f)
	}

	rec, entries, token, up := serve("", bound)
	if rec.Code != http.StatusForbidden || rec.Body.String() != "private_torrent: no_uid\n" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" || up != 0 {
		t.Errorf("bound without uid: %d %q (%s), upstream %d; want 403 \"private_torrent: no_uid\\n\" as text, 0",
			rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"), up)
	}
	refused("bound without uid", entries, token, gateNoUid, "")

	if rec, _, _, up := serve("/"+harnessHash+"/Sintel/Sintel.mkv~vod/hls/0a/s-1-v1-a1.ts", bound); rec.Code != http.StatusForbidden || up != 0 {
		t.Errorf("vod edge, bound without uid: %d, upstream %d; want 403, 0", rec.Code, up)
	}

	asked := rest.hits.Load()
	if rec, _, _, up := serve(ext, jwt.MapClaims{}); rec.Code != http.StatusOK || up != 1 || rest.hits.Load() != asked {
		t.Errorf("/ext/: %d, upstream %d, rest-api asked %d more times; want 200, 1, 0", rec.Code, up, rest.hits.Load()-asked)
	}

	down := newFakeRestAPI(t, restAnswer(500, `{}`))
	h.web.gate = newPrivateGate(down.URL, true, time.Second, time.Minute)
	rec, entries, token, up = serve("", boundUid)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.Len() != 0 || up != 0 {
		t.Errorf("rest-api down: %d %q, upstream %d; want 503 without a body, 0", rec.Code, rec.Body.String(), up)
	}
	refused("rest-api down", entries, token, gateLookupError, "rest-api 500")

	h.web.gate = nil
	if rec, _, _, up := serve("", jwt.MapClaims{}); rec.Code != http.StatusOK || up != 1 {
		t.Errorf("no gate: %d, upstream %d; want 200, 1", rec.Code, up)
	}
}

// TestGateObservePassesEverything: in observe the gate never changes the
// answer, whatever the outcome, an outage and a malformed hash included: the
// request goes to the service, nothing is refused, and the closing line and
// the counter carry the outcome. The closing line names no public outcome:
// that would be almost every request.
func TestGateObservePassesEverything(t *testing.T) {
	h, upstream := countingUpstream(t)
	for _, c := range []struct {
		name, body, path, outcome, cause string
		status                           int
		claims                           jwt.MapClaims
	}{
		{"lookup error", `{}`, "", gateLookupError, "rest-api 500", 500, jwt.MapClaims{"hash": harnessHash, "uid": harnessUid}},
		{"no uid", `{"private":true}`, "", gateNoUid, "", 200, jwt.MapClaims{"hash": harnessHash}},
		{"malformed hash", `{"private":true}`, "/" + harnessHash[:39] + "/f", gateMalformedHash, "", 200, jwt.MapClaims{}},
		{"public", `{"private":false}`, "", gatePublic, "", 200, jwt.MapClaims{}},
	} {
		rest := newFakeRestAPI(t, restAnswer(c.status, c.body))
		h.web.gate = newPrivateGate(rest.URL, false, time.Second, time.Minute)
		r := h.request(t, c.claims, true, "")
		if c.path != "" {
			r.URL.Path = c.path
		}
		before, up := gateCounts(t), upstream.Load()
		rec, entries, _ := h.serveLogged(t, r)
		if rec.Code != http.StatusOK || upstream.Load() != up+1 || len(gateRefusals(entries)) != 0 {
			t.Errorf("%s: %d, upstream %d, %d refusals; want 200, 1, 0", c.name, rec.Code, upstream.Load()-up, len(gateRefusals(entries)))
		}
		logged := c.outcome
		if logged == gatePublic {
			logged = ""
		}
		f := closingFields(entries)
		if f == nil || !fieldIs(f, "private_gate", logged) || !fieldIs(f, "private_gate_cause", c.cause) {
			t.Errorf("%s: closing line %v, want private_gate %q, private_gate_cause %q", c.name, f, logged, c.cause)
		}
		checkCounted(t, before, c.outcome)
	}
}
