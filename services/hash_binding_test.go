package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v4"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// A token bound to a torrent by its hash claim (web-ui binds a free viewer's
// primary whenever grace is on) opens that torrent and no other, and an
// /ext/<url> source, which names no torrent: a subtitle the viewer uploaded as
// srt2vtt converts it (~vtt) and as srt2vtt then reads it through thp on the
// same token (the plain /ext/ request). Neither a first segment that names
// another service (the InfoHash is then that name, so even the token's own
// hash after it is refused) nor ~ext on a torrent's file is a way around the
// binding. A grace token stays bound on /ext/ too: web-ui never sends one
// there, and external-proxy would relay any URL at its rate.
func TestHashBindingCoversTorrentsOnly(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
	// The harness's map is the parser's and the resolver's.
	(*h.web.parser.configs)["vtt"] = &ServiceConfig{Name: throttleTestSvc, EndpointsProvider: Environment}
	tok := signToken(t, h.secret, jwt.MapClaims{"role": "free", "sessionID": "s-bound", "hash": harnessHash})
	grace := signToken(t, h.secret, jwt.MapClaims(graceClaims(harnessHash)))
	other := strings.Repeat("a", 40)
	// base64 of https://example.org/sub.srt, as web-ui's AttachExternalFile
	// builds it.
	const ext = "/ext/aHR0cHM6Ly9leGFtcGxlLm9yZy9zdWIuc3J0/sub.srt"
	hook := logtest.NewGlobal()
	defer hook.Reset()
	for _, c := range []struct {
		path, tok string
		want      int
	}{
		{"/" + harnessHash + "/Sintel/Sintel.mkv", tok, http.StatusOK},
		{"/" + other + "/Sintel/Sintel.mkv", tok, http.StatusForbidden},
		{ext + "~vtt/sub.vtt", tok, http.StatusOK},
		{ext, tok, http.StatusOK},
		{"/vtt/" + harnessHash + "/Sintel/sub.srt", tok, http.StatusForbidden},
		{"/" + other + "/Sintel/poster.png~ext/poster.png", tok, http.StatusForbidden},
		{"/" + harnessHash + "/Sintel/Sintel.mkv", grace, http.StatusOK},
		{ext, grace, http.StatusForbidden},
	} {
		hook.Reset()
		rec := httptest.NewRecorder()
		h.web.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path+"?token="+c.tok+"&api-key=k", nil))
		if rec.Code != c.want {
			t.Errorf("%s (grace token %v): status %d, want %d", c.path, c.tok == grace, rec.Code, c.want)
		}
		mismatch := false
		for _, e := range hook.AllEntries() {
			mismatch = mismatch || e.Message == "token hash mismatch"
		}
		if want := c.want == http.StatusForbidden; mismatch != want {
			t.Errorf("%s (grace token %v): token hash mismatch logged %v, want %v", c.path, c.tok == grace, mismatch, want)
		}
	}
}

// TestGraceRewriteSkipsExtSources: a free viewer's primary carries the grace
// rule and passes on /ext/, where the body is any URL's. The rewrite reads
// its torrent's playlists only: an /ext/ .m3u8 comes back as the upstream
// sent it, the same playlist from the torrent comes back rewritten (so the
// first half cannot pass for want of a rewrite at all).
func TestGraceRewriteSkipsExtSources(t *testing.T) {
	var tok string
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, m3u8("#EXTM3U", "#EXTINF:4.0,", "seg0.ts?token="+tok))
	})
	c := claimsWithGrace(1200)
	c["sessionID"], c["hash"] = "s-bound", harnessHash
	tok = signToken(t, h.secret, c)
	for _, p := range []struct {
		path      string
		rewritten bool
	}{
		// base64 of https://example.org/x.m3u8.
		{"/ext/aHR0cHM6Ly9leGFtcGxlLm9yZy94Lm0zdTg=/x.m3u8", false},
		{"/" + harnessHash + "/Sintel/x.m3u8", true},
	} {
		rec := httptest.NewRecorder()
		h.web.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p.path+"?token="+tok+"&api-key=k", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", p.path, rec.Code)
		}
		if got := strings.Contains(rec.Body.String(), "token="+graceJWT); got != p.rewritten {
			t.Errorf("%s: grace token in the playlist %v, want %v:\n%s", p.path, got, p.rewritten, rec.Body.String())
		}
	}
}
