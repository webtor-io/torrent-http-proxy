package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// A token and a key as they look: a JWT, and a key with dashes.
const (
	redactJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlIjoicGFpZCIsInNlc3Npb25JRCI6InMtMSJ9.c2lnbmF0dXJlLXdyb25nLXdyb25n"
	redactKey = "8acbcf1e-732c-4574-a3bf-27e6f85e2eb1"
)

func TestRedactURL(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"the query",
			"/Sintel/Sintel.mkv?api-key=" + redactKey + "&download=true&token=" + redactJWT,
			"/Sintel/Sintel.mkv?api-key=<redacted>&download=true&token=<redacted>"},
		{"a URL passed on, encoded once (the request's URL field)",
			"/80d7a3c8%2FSintel.mkv%3Fapi-key%3D" + redactKey + "%26download%3Dtrue%26token%3D" + redactJWT,
			"/80d7a3c8%2FSintel.mkv%3Fapi-key%3D<redacted>%26download%3Dtrue%26token%3D<redacted>"},
		{"encoded twice",
			"u=http%253A%252F%252Fx%252Fa.mkv%253Ftoken%253D" + redactJWT + "%2526x%253D1",
			"u=http%253A%252F%252Fx%252Fa.mkv%253Ftoken%253D<redacted>%2526x%253D1"},
		{"a JWT in a path segment",
			"/stremio/resolve/" + redactJWT,
			"/stremio/resolve/<redacted>"},
		{"any case",
			"/a.mkv?Token=" + redactJWT + "&API-KEY=" + redactKey,
			"/a.mkv?Token=<redacted>&API-KEY=<redacted>"},
		{"names that only look like one",
			"/tokenizer=abc/a.mkv?mytoken=abc&tokens=x&api-keys=y",
			"/tokenizer=abc/a.mkv?mytoken=abc&tokens=x&api-keys=y"},
		{"nothing to hide",
			"/80d7a3c8/Sintel/Sintel.mkv~hls/index.m3u8?download=true",
			"/80d7a3c8/Sintel/Sintel.mkv~hls/index.m3u8?download=true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := redactURL(c.in); got != c.want {
				t.Errorf("redactURL(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// Through the server: a content request whose token fails -- the
// "failed to get claims" line, ~5k an hour -- keeps neither the token nor
// the key in any field, and still says which file it was.
func TestRequestLogKeepsNoCredentials(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	h.web.claims = &Claims{apiKey: redactKey, apiSecret: statsSecret}
	hook := logtest.NewGlobal()
	defer hook.Reset()

	// Both shapes of the logged line: the credentials in the query, and --
	// what the hot-linking sites send -- the whole link percent-encoded in
	// the path, which the parser decodes into Path, query and all.
	for _, target := range []string{
		"/" + harnessHash + "/Sintel/Sintel.mkv?api-key=" + redactKey + "&download=true&token=" + redactJWT,
		"/" + harnessHash + "%2FSintel%2FSintel.mkv%3Fapi-key%3D" + redactKey + "%26download%3Dtrue%26token%3D" + redactJWT +
			"?api-key=" + redactKey + "&token=" + redactJWT,
	} {
		hook.Reset()
		rec := httptest.NewRecorder()
		h.web.newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", target, rec.Code)
		}
		var claimsLine *logrus.Entry
		for _, e := range hook.AllEntries() {
			if strings.HasPrefix(e.Message, "failed to get claims") {
				claimsLine = e
			}
			for k, v := range e.Data {
				s := fmt.Sprint(v)
				if strings.Contains(s, redactJWT[:40]) || strings.Contains(s, redactKey) {
					t.Errorf("%q: field %s keeps a credential: %s", e.Message, k, s)
				}
			}
		}
		if claimsLine == nil {
			t.Fatalf("%s: no failed-to-get-claims line", target)
		}
		if p := fmt.Sprint(claimsLine.Data["Path"]); !strings.Contains(p, "Sintel.mkv") {
			t.Errorf("Path %q: the file should still be there", p)
		}
	}
}

// A request served: its line keeps the Referer, which can be a page whose
// own URL carries the key and the token -- redacted like the rest.
func TestServedLogKeepsNoCredentials(t *testing.T) {
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	h.web.claims = &Claims{apiKey: redactKey, apiSecret: statsSecret}
	tok := signToken(t, statsSecret, viewerClaims(time.Now().Add(time.Hour).Unix()))
	hook := logtest.NewGlobal()
	defer hook.Reset()

	r := httptest.NewRequest(http.MethodGet, "/"+harnessHash+"/Sintel/Sintel.mkv?api-key="+redactKey+"&token="+tok, nil)
	r.Header.Set("Referer", "https://example.org/watch?api-key="+redactKey+"&token="+tok)
	rec := httptest.NewRecorder()
	h.web.newMux().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	served := false
	for _, e := range hook.AllEntries() {
		if e.Message == "request served successfully" {
			served = true
			if ref := fmt.Sprint(e.Data["referer"]); !strings.Contains(ref, "example.org/watch") {
				t.Errorf("referer %q: the page should still be there", ref)
			}
		}
		for k, v := range e.Data {
			s := fmt.Sprint(v)
			if strings.Contains(s, tok[:40]) || strings.Contains(s, redactKey) {
				t.Errorf("%q: field %s keeps a credential: %s", e.Message, k, s)
			}
		}
	}
	if !served {
		t.Fatal("no request-served line")
	}
}
