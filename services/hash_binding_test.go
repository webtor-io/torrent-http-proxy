package services

import (
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
// another service nor ~ext on a torrent's file is a way around the binding.
func TestHashBindingCoversTorrentsOnly(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 3))
	// The harness's map is the parser's and the resolver's.
	(*h.web.parser.configs)["vtt"] = &ServiceConfig{Name: throttleTestSvc, EndpointsProvider: Environment}
	tok := signToken(t, h.secret, jwt.MapClaims{"role": "free", "sessionID": "s-bound", "hash": harnessHash})
	other := strings.Repeat("a", 40)
	// base64 of https://example.org/sub.srt, as web-ui's AttachExternalFile
	// builds it.
	const ext = "/ext/aHR0cHM6Ly9leGFtcGxlLm9yZy9zdWIuc3J0/sub.srt"
	hook := logtest.NewGlobal()
	defer hook.Reset()
	for _, c := range []struct {
		path string
		want int
	}{
		{"/" + harnessHash + "/Sintel/Sintel.mkv", http.StatusOK},
		{"/" + other + "/Sintel/Sintel.mkv", http.StatusForbidden},
		{ext + "~vtt/sub.vtt", http.StatusOK},
		{ext, http.StatusOK},
		{"/vtt/" + other + "/Sintel/sub.srt", http.StatusForbidden},
		{"/" + other + "/Sintel/poster.png~ext/poster.png", http.StatusForbidden},
	} {
		hook.Reset()
		rec := httptest.NewRecorder()
		h.web.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path+"?token="+tok+"&api-key=k", nil))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.path, rec.Code, c.want)
		}
		mismatch := false
		for _, e := range hook.AllEntries() {
			mismatch = mismatch || e.Message == "token hash mismatch"
		}
		if want := c.want == http.StatusForbidden; mismatch != want {
			t.Errorf("%s: token hash mismatch logged %v, want %v", c.path, mismatch, want)
		}
	}
}
