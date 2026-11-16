package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
)

// navigate marks r as a browser's top-level navigation.
func navigate(r *http.Request) *http.Request {
	r.Header.Set("Sec-Fetch-Dest", "document")
	return r
}

// checkPage fails unless rec is a refusal page with status and title.
func checkPage(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, title string) {
	t.Helper()
	h := rec.Header()
	if rec.Code != status {
		t.Errorf("%s: %d, want %d", name, rec.Code, status)
	}
	for k, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'",
		"Cache-Control":           "no-store",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", name, k, got, want)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="en">`) || !strings.Contains(body, title) {
		t.Errorf("%s: the page has no lang=en or no title %q:\n%s", name, title, body)
	}
	if len(body) > 2048 {
		t.Errorf("%s: page is %d bytes, want at most 2048 (net/http sets Content-Length only then)", name, len(body))
	}
}

// refusalSite is one place thp refuses a request: req builds a fresh
// request to it on h, plain is the body a non-navigation gets ("" for a bare
// status).
type refusalSite struct {
	name   string
	status int
	title  string
	plain  string
	setup  func(t *testing.T) (h *throttleHarness, req func() *http.Request)
}

// gated is a harness with the private gate enforcing on rest-api's answer.
func gated(t *testing.T, status int, body string) *throttleHarness {
	t.Helper()
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
	rest := newFakeRestAPI(t, restAnswer(status, body))
	h.web.gate = newPrivateGate(rest.URL, true, time.Second, time.Minute)
	return h
}

// onPath is h.request for claims at path ("" is the harness's file).
func onPath(t *testing.T, h *throttleHarness, path string, claims jwt.MapClaims) func() *http.Request {
	return func() *http.Request {
		r := h.request(t, claims, true, "")
		if path != "" {
			r.URL.Path = path
		}
		return r
	}
}

func refusalSites() []refusalSite {
	file := "/" + harnessHash + "/Sintel/Sintel.mkv"
	return []refusalSite{
		{"claims", http.StatusForbidden, pageLinkInvalid.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			return h, func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, file+"?token=not-a-token&api-key=k", nil)
				r.Header.Set("X-Forwarded-For", "203.0.113.7")
				return r
			}
		}},
		{"hash mismatch", http.StatusForbidden, pageLinkInvalid.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			return h, onPath(t, h, "", jwt.MapClaims{"hash": strings.Repeat("a", 40)})
		}},
		{"gate unbound", http.StatusForbidden, pagePrivate.title, "private_torrent: unbound\n", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 200, `{"private":true}`)
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"gate no uid", http.StatusForbidden, pagePrivate.title, "private_torrent: no_uid\n", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 200, `{"private":true}`)
			return h, onPath(t, h, "", jwt.MapClaims{"hash": harnessHash})
		}},
		{"gate malformed hash", http.StatusForbidden, pageLinkInvalid.title, "private_torrent: malformed_hash\n", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 200, `{"private":true}`)
			return h, onPath(t, h, "/"+harnessHash[:39]+"/f", jwt.MapClaims{})
		}},
		{"gate lookup 403", http.StatusForbidden, pageNotAvailable.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 403, `{}`)
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"gate lookup 404", http.StatusNotFound, pageTorrentNotFound.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 404, `{}`)
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"gate lookup 503", http.StatusServiceUnavailable, pageTemporary.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := gated(t, 500, `{}`)
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"session IP", http.StatusTooManyRequests, pageOtherNetwork.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			h.web.enforceSessionIP = true
			return h, onPath(t, h, "", jwt.MapClaims{"sessionID": "s-ip", "remoteAddress": "198.51.100.9"})
		}},
		{"limiter", http.StatusTooManyRequests, "Too many downloads at once", "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := refusalHarness(t, "s-page", 0)
			return h, onPath(t, h, "", tierClaims("t-page", "s-page", "2M"))
		}},
		{"missing source", http.StatusNotFound, pageNotFound.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, http.NotFound)
			withVodEdge(t, h)
			claims := tierClaims("t-miss", "s-miss", "2M")
			// nginx-vod's own read is 404: the session's segments are now
			// answered by thp, not by the upstream's text/plain 404.
			h.web.newMux().ServeHTTP(httptest.NewRecorder(), h.request(t, claims, false, ""))
			return h, onPath(t, h, file+"~vod/hls/0a/s-1-v1-a1.ts", claims)
		}},
		{"url parse", http.StatusBadRequest, pageLinkBroken.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			return h, onPath(t, h, "/nothash/Sintel.mkv", jwt.MapClaims{})
		}},
		{"no service", http.StatusNotImplemented, pageLinkBroken.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			key := os.Getenv("THP_THROTTLE_TEST_SERVICE_HOST") + ":" + os.Getenv("THP_THROTTLE_TEST_SERVICE_PORT")
			if _, err := h.web.pr.LazyMap.Get(key, func() (*httputil.ReverseProxy, error) { return nil, nil }); err != nil {
				t.Fatal(err)
			}
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"bucket", http.StatusInternalServerError, pageServerError.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			return h, onPath(t, h, "", tierClaims("t-rate", "s-rate", "5X"))
		}},
		{"proxy get", http.StatusInternalServerError, pageServerError.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
			t.Setenv("THP_THROTTLE_TEST_SERVICE_HOST", "not-an-ip")
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
		{"upstream gone", http.StatusBadGateway, pageServerError.title, "", func(t *testing.T) (*throttleHarness, func() *http.Request) {
			h := newThrottleHarness(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
			return h, onPath(t, h, "", jwt.MapClaims{})
		}},
	}
}

// Every refusal of thp's own is today's response unless the request is a
// browser's top-level navigation, which gets the same status with a short
// page: a download manager, an app or a player never gets a body, which curl
// would write into the file.
func TestRefusalPagesOnlyForNavigation(t *testing.T) {
	for _, c := range refusalSites() {
		t.Run(c.name, func(t *testing.T) {
			h, req := c.setup(t)
			mux := h.web.newMux()

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req())
			if rec.Code != c.status {
				t.Errorf("without Sec-Fetch-Dest: %d, want %d", rec.Code, c.status)
			}
			if got := rec.Body.String(); got != c.plain {
				t.Errorf("without Sec-Fetch-Dest: body %q, want %q", got, c.plain)
			}
			wantType := ""
			if c.plain != "" {
				wantType = "text/plain; charset=utf-8"
			}
			if got := rec.Header().Get("Content-Type"); got != wantType {
				t.Errorf("without Sec-Fetch-Dest: Content-Type %q, want %q", got, wantType)
			}
			for _, k := range []string{"Content-Security-Policy", "Cache-Control"} {
				if v := rec.Header().Get(k); v != "" {
					t.Errorf("without Sec-Fetch-Dest: %s = %q, want none", k, v)
				}
			}

			// Not a navigation either: a <video>, a fetch, an iframe.
			r := req()
			r.Header.Set("Sec-Fetch-Dest", "video")
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != c.status || rec.Body.String() != c.plain {
				t.Errorf("Sec-Fetch-Dest video: %d %q, want %d %q", rec.Code, rec.Body.String(), c.status, c.plain)
			}

			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, navigate(req()))
			checkPage(t, "navigation", rec, c.status, c.title)
		})
	}
}

// A navigation the limiter refused is held and carries Retry-After like any
// other refusal, and counts once.
func TestRefusalPageKeepsLimiterHoldAndRetryAfter(t *testing.T) {
	const delay = 200 * time.Millisecond
	h := refusalHarness(t, "s-page-held", delay)
	before := rejectedTotal(t, "total")

	start := time.Now()
	rec, _, _ := h.serveRaw(t, navigate(h.request(t, tierClaims("t-page-held", "s-page-held", "2M"), true, "")))
	took := time.Since(start)

	checkPage(t, "limiter", rec, http.StatusTooManyRequests, h.web.sl.refusalPage("total").title)
	if got := rec.Header().Get("Retry-After"); got != limiterRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, limiterRetryAfter)
	}
	if took < delay {
		t.Errorf("answered after %v, want the %v hold first", took, delay)
	}
	if n := rejectedTotal(t, "total") - before; n != 1 {
		t.Errorf("rejected_total{reason=total} grew by %v, want 1", n)
	}
}

// Each cap's page names the configured number, not a constant.
func TestLimiterPagesUseConfiguredCaps(t *testing.T) {
	l := &SessionLimiter{maxPerPath: 7, maxTotal: 31, maxBigFilesPerHash: 3, maxIPsPerSession: 4}
	for reason, n := range map[string]string{"path": "7", "total": "31", "bigfiles": "3", "ips": "4"} {
		p := l.refusalPage(reason)
		if !strings.Contains(p.text, " "+n+" ") {
			t.Errorf("%s: %q does not name %s", reason, p.text, n)
		}
		if p == pageTooMany {
			t.Errorf("%s: the generic page", reason)
		}
	}
	if p := l.refusalPage("something new"); p != pageTooMany {
		t.Errorf("unknown reason: %+v, want the generic page", p)
	}
}

// Nothing from the request reaches a page: it renders on the content host.
func TestRefusalPageEchoesNothing(t *testing.T) {
	const marker = "ZZFILEMARKER"
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
	mux := h.web.newMux()
	for _, target := range []string{
		"/" + harnessHash + "/%3Cscript%3Ex%3C%2Fscript%3E/" + marker + ".mkv?token=" + marker + "&api-key=k&q=%3Cscript%3Ex%3C%2Fscript%3E",
		"/nothash/%3Cscript%3Ex%3C%2Fscript%3E/" + marker + ".mkv?token=" + marker,
	} {
		r := navigate(httptest.NewRequest(http.MethodGet, target, nil))
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want a refusal", target, rec.Code)
		}
		body := rec.Body.String()
		if body == "" || strings.Contains(body, marker) || strings.Contains(body, "<script>") || strings.Contains(body, "script%3E") {
			t.Errorf("%s: the page is empty or echoes the request:\n%s", target, body)
		}
	}
}

// HEAD on a navigation: the page's status and headers, Content-Length that
// of the GET's page, no body, over a real connection.
func TestRefusalPageHEAD(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
	srv := httptest.NewServer(h.web.newMux())
	t.Cleanup(srv.Close)
	do := func(method string) (*http.Response, []byte) {
		t.Helper()
		r, err := http.NewRequest(method, srv.URL+"/"+harnessHash+"/Sintel/Sintel.mkv?token=bad&api-key=k", nil)
		if err != nil {
			t.Fatal(err)
		}
		navigate(r)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res, b
	}
	get, page := do(http.MethodGet)
	head, body := do(http.MethodHead)
	if get.StatusCode != http.StatusForbidden || len(page) == 0 || get.ContentLength != int64(len(page)) {
		t.Fatalf("GET: %d, %d bytes, Content-Length %d; want 403 with a page and its length", get.StatusCode, len(page), get.ContentLength)
	}
	if head.StatusCode != http.StatusForbidden || len(body) != 0 || head.ContentLength != int64(len(page)) {
		t.Errorf("HEAD: %d, %d bytes, Content-Length %d; want 403, none, %d", head.StatusCode, len(body), head.ContentLength, len(page))
	}
	if ct := head.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("HEAD: Content-Type %q", ct)
	}
}

// errorHandler answers 502 both ways and a body only to a navigation. A
// client that left is recorded as before and nothing panics.
func TestErrorHandlerPageOnlyForNavigation(t *testing.T) {
	for _, nav := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/"+harnessHash+"/Sintel/Sintel.mkv", nil)
		if nav {
			navigate(r)
		}
		rec := httptest.NewRecorder()
		errorHandler(rec, r, errors.New("dial tcp: connection refused"))
		if nav {
			checkPage(t, "navigation", rec, http.StatusBadGateway, pageServerError.title)
		} else if rec.Code != http.StatusBadGateway || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
			t.Errorf("not a navigation: %d %q (%s), want a bare 502", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
		}

		client, cancel := context.WithCancel(context.Background())
		cancel()
		o := &proxyOutcome{client: client}
		r = withProxyOutcome(r.WithContext(client), o)
		rec = httptest.NewRecorder()
		errorHandler(rec, r, context.Canceled)
		if rec.Code != http.StatusBadGateway || !o.clientGone {
			t.Errorf("client gone (navigation %v): %d, clientGone %v; want 502, true", nav, rec.Code, o.clientGone)
		}
	}
}
