package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestGateUploadUpperCaseRefusedAtBinding: kind 0 in upper case with the
// owner's token (hash in lower case, as web-ui mints it) is refused at the
// byte-wise hash binding before the gate: a bare 403, nothing upstream, and
// rest-api never asked. Observe does not let kind 0 through.
func TestGateUploadUpperCaseRefusedAtBinding(t *testing.T) {
	h, upstream := countingUpstream(t)
	rest := newFakeRestAPI(t, restAnswer(200, `{"private":false}`))
	h.web.gate = newPrivateGate(rest.URL, false, time.Second, time.Minute) // observe
	serve := func(id string, claims jwt.MapClaims) (*httptest.ResponseRecorder, []*logrus.Entry, int32) {
		r := h.request(t, claims, true, "")
		r.URL.Path = "/" + id + "/Name/a.mkv"
		before := upstream.Load()
		rec, entries, _ := h.serveLogged(t, r)
		return rec, entries, upstream.Load() - before
	}
	owner := jwt.MapClaims{"hash": uploadKind0, "uid": harnessUid}

	rec, entries, up := serve(strings.ToUpper(uploadKind0), owner)
	mismatch := false
	for _, e := range entries {
		mismatch = mismatch || e.Message == "token hash mismatch"
	}
	if rec.Code != http.StatusForbidden || rec.Body.Len() != 0 || up != 0 || !mismatch {
		t.Errorf("upper case, owner: %d %q, upstream %d, hash mismatch logged %v; want 403 without a body, 0, true", rec.Code, rec.Body.String(), up, mismatch)
	}
	if rec, _, up := serve(uploadKind0, owner); rec.Code != http.StatusOK || up != 1 {
		t.Errorf("owner: %d, upstream %d; want 200, 1", rec.Code, up)
	}
	if rec, _, up := serve(uploadKind0, jwt.MapClaims{"hash": uploadKind0}); rec.Code != http.StatusForbidden || rec.Body.String() != "private_torrent: no_uid\n" || up != 0 {
		t.Errorf("bound without uid: %d %q, upstream %d; want 403 \"private_torrent: no_uid\\n\", 0", rec.Code, rec.Body.String(), up)
	}
	if n := rest.hits.Load(); n != 0 {
		t.Errorf("rest-api asked %d times, want 0", n)
	}
}

// TestGateUploadServiceWrite: web-ui's two calls without a user, delete and
// seal, carry the same claims and differ by the method only; both reach the
// seeder as they came.
func TestGateUploadServiceWrite(t *testing.T) {
	type seen struct{ method, path, infoHash string }
	var (
		mu  sync.Mutex
		got []seen
	)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("X-Info-Hash")})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	received := func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), got...)
	}
	rest := newFakeRestAPI(t, restAnswer(200, `{"private":true}`))
	h.web.gate = newPrivateGate(rest.URL, true, time.Second, time.Minute) // enforce
	send := func(method string, claims jwt.MapClaims) (*httptest.ResponseRecorder, []seen) {
		r := h.request(t, claims, true, "")
		r.Method = method
		r.URL.Path = "/" + uploadKind0 + "/"
		before := len(received())
		rec, _, _ := h.serveLogged(t, r)
		return rec, received()[before:]
	}

	service := jwt.MapClaims{"hash": uploadKind0, "internal": true, "up": "add", "lim": 1.0}
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		rec, reqs := send(m, service)
		if rec.Code != http.StatusNoContent || len(reqs) != 1 || reqs[0] != (seen{m, "/", uploadKind0}) {
			t.Errorf("%s, service token: %d, upstream got %+v; want 204, one {%s / %s}", m, rec.Code, reqs, m, uploadKind0)
		}
	}
	rec, reqs := send(http.MethodPut, jwt.MapClaims{"hash": uploadKind0, "up": "add", "lim": 1.0})
	if rec.Code != http.StatusForbidden || rec.Body.String() != "private_torrent: no_uid\n" || len(reqs) != 0 {
		t.Errorf("PUT without internal: %d %q, upstream got %+v; want 403 \"private_torrent: no_uid\\n\", nothing", rec.Code, rec.Body.String(), reqs)
	}
	if n := rest.hits.Load(); n != 0 {
		t.Errorf("rest-api asked %d times, want 0", n)
	}
}

// TestGateUploadPaddedIDRefusedInObserve: a space or a tab at either end of a
// kind 0 ID makes it 41 characters to the gate, malformed_hash, which observe
// lets through, and net/http trims a header value, so the upstream would get
// X-Info-Hash as the upload's ID itself. Observe refuses it like the ID, on
// any token that names no hash; nothing goes upstream, rest-api is not asked.
// A torrent's hash spelled so passes observe as before.
func TestGateUploadPaddedIDRefusedInObserve(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Info-Hash"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	rest := newFakeRestAPI(t, restAnswer(200, `{"private":false}`))
	h.web.gate = newPrivateGate(rest.URL, false, time.Second, time.Minute) // observe
	serve := func(id string, claims jwt.MapClaims) (*httptest.ResponseRecorder, []string) {
		r := h.request(t, claims, true, "")
		r.URL.Path = "/" + id + "/Name/a.mkv"
		mu.Lock()
		before := len(got)
		mu.Unlock()
		rec, _, _ := h.serveLogged(t, r)
		mu.Lock()
		defer mu.Unlock()
		return rec, append([]string(nil), got[before:]...)
	}
	for _, id := range []string{" " + uploadKind0, uploadKind0 + " ", "\t" + uploadKind0, uploadKind0 + "\t"} {
		for _, claims := range []jwt.MapClaims{{}, {"uid": "another-user"}} {
			rec, up := serve(id, claims)
			if rec.Code != http.StatusForbidden || rec.Body.String() != "private_torrent: malformed_hash\n" || len(up) != 0 {
				t.Errorf("%q, claims %v: %d %q, upstream got X-Info-Hash %q; want 403 \"private_torrent: malformed_hash\\n\", nothing", id, claims, rec.Code, rec.Body.String(), up)
			}
		}
	}
	if n := rest.hits.Load(); n != 0 {
		t.Errorf("rest-api asked %d times, want 0", n)
	}
	if rec, up := serve(" "+harnessHash, jwt.MapClaims{}); rec.Code != http.StatusOK || len(up) != 1 {
		t.Errorf("padded torrent hash: %d, upstream got %q; want 200, one request", rec.Code, up)
	}
}
