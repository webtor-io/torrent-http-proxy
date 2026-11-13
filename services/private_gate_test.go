package services

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRestAPI is rest-api's GET /resource/<h> as the gate sees it. URL has no
// trailing "/", as the gate is given it; hits counts the requests that reached
// it, and last gives the request target and X-Token of the latest one.
type fakeRestAPI struct {
	*httptest.Server
	hits       atomic.Int32
	mu         sync.Mutex
	uri, token string
}

func newFakeRestAPI(t *testing.T, h http.HandlerFunc) *fakeRestAPI {
	t.Helper()
	f := &fakeRestAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.uri, f.token = r.RequestURI, r.Header.Get("X-Token")
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRestAPI) last() (uri, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uri, f.token
}

// restAnswer writes status and body, as rest-api answers.
func restAnswer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// TestPrivacyLookupClassification: rest-api's answers to an anonymous
// GET /resource/<h> become private, public or an error with the status the
// gate answers in enforce. Only 200 with the field and the two private codes
// of a 403 are answers; a ban, a missing torrent, an outage and anything
// unparsable are errors, never "public". The cause is fixed text and never
// carries the body.
func TestPrivacyLookupClassification(t *testing.T) {
	const h = harnessHash
	huge := `{"private":false,"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	for _, c := range []struct {
		name       string
		status     int
		body       string
		wantPriv   bool
		wantStatus int // 0: not an error
	}{
		{"public", 200, `{"id":"x","private":false}`, false, 0},
		{"private", 200, `{"private":true}`, true, 0},
		{"refused private", 403, `{"error":"…","code":"private_torrent","reason":"sign_in"}`, true, 0},
		{"store without key", 403, `{"code":"private_unavailable"}`, true, 0},
		{"ban", 403, `{"error":"forbidden"}`, false, 403},
		{"other code", 403, `{"code":"private_torrent_file"}`, false, 403},
		{"not found", 404, `{"error":"not found"}`, false, 404},
		{"server error", 500, `{}`, false, 503},
		{"store outdated", 503, `{"code":"store_outdated"}`, false, 503},
		{"no field", 200, `{"id":"x"}`, false, 503},
		{"null field", 200, `{"private":null}`, false, 503},
		{"garbage", 200, `not json`, false, 503},
		{"huge body", 200, huge, false, 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRestAPI(t, restAnswer(c.status, c.body))
			got, err := newPrivateGate(f.URL, true, time.Second, time.Minute).lookup(h)
			if c.wantStatus == 0 {
				if err != nil || got != c.wantPriv {
					t.Errorf("got (%v, %v), want (%v, nil)", got, err, c.wantPriv)
				}
			} else {
				checkLookupError(t, err, c.wantStatus, "")
			}
			if uri, token := f.last(); uri != "/resource/"+h || token != "" {
				t.Errorf("request %q with X-Token %q, want /resource/%s without a token", uri, token, h)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		f := newFakeRestAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
			restAnswer(200, `{"private":false}`)(w, nil)
		})
		_, err := newPrivateGate(f.URL, true, 50*time.Millisecond, time.Minute).lookup(h)
		checkLookupError(t, err, 503, "rest-api unreachable")
	})
	t.Run("closed server", func(t *testing.T) {
		f := newFakeRestAPI(t, restAnswer(200, `{"private":false}`))
		f.Close()
		_, err := newPrivateGate(f.URL, true, time.Second, time.Minute).lookup(h)
		checkLookupError(t, err, 503, "rest-api unreachable")
	})
}

// checkLookupError: err is a *lookupError with status and, when cause is not
// "", that cause; a cause never carries a JSON body.
func checkLookupError(t *testing.T, err error, status int, cause string) {
	t.Helper()
	var le *lookupError
	if !errors.As(err, &le) {
		t.Fatalf("error %v (%T), want a *lookupError", err, err)
	}
	if le.status != status || (cause != "" && le.cause != cause) || strings.Contains(le.cause, "{") {
		t.Errorf("lookupError{%d, %q}, want status %d, cause %q, no body", le.status, le.cause, status, cause)
	}
}

// TestPrivacyCacheErrorTTL: during a rest-api outage a hash is asked once per
// error TTL, not on every viewer request, and the error expires instead of
// refusing the hash until the pod restarts; an answer is kept for good.
func TestPrivacyCacheErrorTTL(t *testing.T) {
	var failed atomic.Bool
	f := newFakeRestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if failed.CompareAndSwap(false, true) {
			restAnswer(500, `{}`)(w, r)
			return
		}
		restAnswer(200, `{"private":false}`)(w, r)
	})
	g := newPrivateGate(f.URL, true, time.Second, 100*time.Millisecond)
	_, err1 := g.isPrivate(harnessHash)
	_, err2 := g.isPrivate(harnessHash)
	if err1 == nil || err2 == nil {
		t.Fatalf("errors %v, %v, want both (the second cached)", err1, err2)
	}
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("rest-api asked %d times, want 1: the error is cached", n)
	}
	time.Sleep(150 * time.Millisecond)
	if p, err := g.isPrivate(harnessHash); p || err != nil {
		t.Fatalf("after the error TTL got (%v, %v), want (false, nil)", p, err)
	}
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("rest-api asked %d times, want 2: the error expired", n)
	}
	time.Sleep(150 * time.Millisecond)
	if p, err := g.isPrivate(harnessHash); p || err != nil {
		t.Fatalf("cached answer (%v, %v), want (false, nil)", p, err)
	}
	if n := f.hits.Load(); n != 2 {
		t.Errorf("rest-api asked %d times, want 2: an answer does not expire", n)
	}
}

// TestPrivacyLookupOncePerHash: HLS segments of a new hash arrive in a burst;
// rest-api is asked once and every request gets its answer.
func TestPrivacyLookupOncePerHash(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := newFakeRestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		restAnswer(200, `{"private":true}`)(w, r)
	})
	g := newPrivateGate(f.URL, true, time.Second, time.Minute)
	const n = 50
	type result struct {
		p   bool
		err error
	}
	results := make(chan result, n)
	for range n {
		go func() {
			p, err := g.isPrivate(harnessHash)
			results <- result{p, err}
		}()
	}
	<-entered
	// The lookup is in flight; let the rest of the burst reach isPrivate. Were
	// each one to ask, its request would wait here too and count.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range n {
		if r := <-results; !r.p || r.err != nil {
			t.Errorf("got (%v, %v), want (true, nil)", r.p, r.err)
		}
	}
	if got := f.hits.Load(); got != 1 {
		t.Errorf("rest-api asked %d times, want 1", got)
	}
}
