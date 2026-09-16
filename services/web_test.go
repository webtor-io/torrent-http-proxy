package services

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestModHeadersScrubClientValue exercises the same two steps proxyHTTP
// performs before handing the request to the upstream proxy: build the
// header map via setModHeaders, then apply it onto the outgoing request with
// r.Header.Set (proxyHTTP's `for k, v := range headers { r.Header.Set(k, v) }`
// loop). It verifies I4: a mod-less source (e.g. GET /tr/<hash>/... where the
// first path segment merely matches a mod name but Source.Mod is nil) must
// never let a client-supplied X-Mod-Extra reach the upstream service, while a
// real mod (~tr:pt) must still be forwarded.
func TestModHeadersScrubClientValue(t *testing.T) {
	buildUpstreamRequest := func(src *Source) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/tr/deadbeef", nil)
		// Simulate a hostile client trying to smuggle a mod header directly.
		r.Header.Set("X-Mod-Type", "evil-type")
		r.Header.Set("X-Mod-Extra", "evil")

		headers := map[string]string{}
		setModHeaders(headers, src)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	t.Run("no mod: client header is scrubbed", func(t *testing.T) {
		r := buildUpstreamRequest(&Source{InfoHash: "deadbeef", Path: "/"})
		if got := r.Header.Get("X-Mod-Extra"); got != "" {
			t.Fatalf("expected upstream to see no X-Mod-Extra, got %q", got)
		}
		if got := r.Header.Get("X-Mod-Type"); got != "" {
			t.Fatalf("expected upstream to see no X-Mod-Type, got %q", got)
		}
	})

	t.Run("~tr:pt: real mod value is forwarded", func(t *testing.T) {
		r := buildUpstreamRequest(&Source{
			InfoHash: "deadbeef",
			Path:     "/",
			Mod:      &Mod{Type: "tr", Extra: "pt", Name: "subtitle-translate"},
		})
		if got := r.Header.Get("X-Mod-Extra"); got != "pt" {
			t.Fatalf("expected upstream to see X-Mod-Extra=pt, got %q", got)
		}
		if got := r.Header.Get("X-Mod-Type"); got != "tr" {
			t.Fatalf("expected upstream to see X-Mod-Type=tr, got %q", got)
		}
	})
}
