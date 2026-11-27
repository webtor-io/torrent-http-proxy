package services

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// withVodEdge routes h's vod edge to the harness's caller service on the
// harness's upstream.
func withVodEdge(t *testing.T, h *throttleHarness) {
	t.Helper()
	(*h.web.parser.configs)["vod"] = &ServiceConfig{Name: harnessCallerSvc, EndpointsProvider: Environment}
	t.Setenv("NGINX_VOD_SERVICE_HOST", os.Getenv("THP_THROTTLE_TEST_SERVICE_HOST"))
	t.Setenv("NGINX_VOD_SERVICE_PORT", os.Getenv("THP_THROTTLE_TEST_SERVICE_PORT"))
}

// harnessUid is the uid claim of the tests' tokens.
const harnessUid = "0123456789abcdef0123456789abcdef01234567"

// TestUpstreamUidFromVerifiedToken: the seeder picks the account a private
// torrent downloads under by X-Uid, so every edge's upstream sees the
// verified token's uid, "" without the claim, and never what a client wrote.
// The vod edge is the harness's caller service on the same upstream, as in
// TestMissingSourceAnswered404. The cases share one ReverseProxy, in order: a
// uid must not carry over to the next request.
func TestUpstreamUidFromVerifiedToken(t *testing.T) {
	seen := make(chan string, 1)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- strings.Join(r.Header.Values("X-Uid"), ",")
	})
	withVodEdge(t, h)
	exp := time.Now().Add(time.Hour).Unix()
	withUid, noUid := jwt.MapClaims{"uid": harnessUid, "exp": exp}, jwt.MapClaims{"exp": exp}
	const segment = "~vod/hls/0a/s-1-v1-a1.ts"
	for _, c := range []struct {
		name, suffix, edge, clientUid, want string
		claims                              jwt.MapClaims
	}{
		{"seeder, token uid", "", throttleTestSvc, "", harnessUid, withUid},
		{"seeder, token uid, client evil", "", throttleTestSvc, "evil", harnessUid, withUid},
		{"seeder, no claim, client evil", "", throttleTestSvc, "evil", "", noUid},
		{"vod edge, token uid, client evil", segment, harnessCallerSvc, "evil", harnessUid, withUid},
		{"vod edge, no claim, client evil", segment, harnessCallerSvc, "evil", "", noUid},
	} {
		r := h.request(t, c.claims, true, "")
		r.URL.Path += c.suffix
		if c.clientUid != "" {
			r.Header.Set("X-Uid", c.clientUid)
		}
		rec := httptest.NewRecorder()
		fields := h.serve(t, rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.name, rec.Code)
			continue
		}
		if fields["edge"] != c.edge {
			t.Errorf("%s: edge %v, want %s", c.name, fields["edge"], c.edge)
		}
		if got := <-seen; got != c.want {
			t.Errorf("%s: upstream X-Uid %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRetryKeepsUpstreamUid: without X-Uid among the headers the retry
// copies, a private torrent's read resumes under no account.
func TestRetryKeepsUpstreamUid(t *testing.T) {
	retryKeepsUpstreamHeader(t, map[string]any{"uid": harnessUid, "exp": time.Now().Add(time.Hour).Unix()}, "X-Uid", harnessUid)
}

// TestUpstreamWriteClaimsFromVerifiedToken: the seeder takes what a write
// token allows from X-Upload-Up, X-Upload-Lim and X-Internal without reading
// the token, so the upstream sees them only from a token verified under the
// API secret that carries exp (golang-jwt checks exp only when it is there),
// and never what a client wrote. The cases share one ReverseProxy, in order:
// a value must not carry over to the next request.
func TestUpstreamWriteClaimsFromVerifiedToken(t *testing.T) {
	headers := []string{"X-Upload-Up", "X-Upload-Lim", "X-Internal"}
	seen := make(chan [3]string, 1)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var got [3]string
		for i, k := range headers {
			got[i] = strings.Join(r.Header.Values(k), ",")
		}
		seen <- got
	})
	exp := time.Now().Add(time.Hour).Unix()
	none := [3]string{}
	check := func(name string, claims jwt.MapClaims, clientEvil bool, want [3]string) {
		t.Helper()
		r := h.request(t, claims, true, "")
		if clientEvil {
			r.Header.Set("X-Upload-Up", "new")
			r.Header.Set("X-Upload-Lim", "999999999999")
			r.Header.Set("X-Internal", "true")
		}
		rec := httptest.NewRecorder()
		h.serve(t, rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, rec.Code)
			return
		}
		if got := <-seen; got != want {
			t.Errorf("%s: upstream %v %q, want %q", name, headers, got, want)
		}
	}
	check("service token", jwt.MapClaims{"up": "add", "lim": 6442450944, "internal": true, "exp": exp}, true, [3]string{"add", "6442450944", "true"})
	check("owner token", jwt.MapClaims{"up": "new", "lim": 1, "uid": harnessUid, "exp": exp}, true, [3]string{"new", "1", ""})
	check("no write claims, client evil", jwt.MapClaims{"uid": harnessUid, "exp": exp}, true, none)
	check("internal not a JSON true", jwt.MapClaims{"up": "add", "lim": 1, "internal": "true", "exp": exp}, false, [3]string{"add", "1", ""})
	check("lim not a whole number", jwt.MapClaims{"up": "add", "lim": 1.5, "internal": true, "exp": exp}, false, [3]string{"add", "", "true"})
	check("no exp", jwt.MapClaims{"up": "add", "lim": 1, "internal": true}, true, none)

	// An API key without a secret: Get checks the signature under an empty
	// key, which anyone can sign with.
	h.web.claims, h.secret = &Claims{apiKey: "k"}, ""
	check("empty secret", jwt.MapClaims{"up": "add", "lim": 1, "internal": true, "exp": exp}, true, none)
	// No key and no secret: Get takes no token at all.
	h.web.claims = &Claims{}
	check("no auth, client evil", jwt.MapClaims{"up": "add", "lim": 1, "internal": true, "exp": exp}, true, none)
}
