package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// serveToken sends an external request for the harness's torrent with tok,
// and clientRole as the client's own X-Role when not "", through proxyHTTP.
// Not serve: a refused token has no closing log line to wait for.
func (h *throttleHarness) serveToken(t *testing.T, tok, clientRole string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/"+harnessHash+"/Sintel/Sintel.mkv?token="+tok+"&api-key=k", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if clientRole != "" {
		r.Header.Set("X-Role", clientRole)
	}
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	rec := httptest.NewRecorder()
	logger, _ := logtest.NewNullLogger()
	h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))
	return rec
}

// TestUpstreamRoleIsTheTokens: the seeder skips Vault's copy for X-Role
// vault, so the upstream must see the verified token's role and nothing a
// client wrote. The cases share one harness, and so one ReverseProxy, in
// order: a role must not carry over to the next request.
func TestUpstreamRoleIsTheTokens(t *testing.T) {
	seen := make(chan string, 1)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- strings.Join(r.Header.Values("X-Role"), ",")
	})
	exp := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name       string
		payload    map[string]any
		clientRole string
		want       string
	}{
		{"role vault", map[string]any{"role": "vault", "exp": exp}, "", "vault"},
		{"role free, client says vault", map[string]any{"role": "free", "exp": exp}, "vault", "free"},
		{"no role, client says vault", map[string]any{"exp": exp}, "vault", ""},
		// What Vault signs (jwt v5 RegisteredClaims, omitempty: no exp), and
		// rest-api passes on into the export URL unchanged.
		{"Vault's token, no exp", map[string]any{"role": "vault", "sessionID": "", "agent": "", "remoteAddress": ""}, "", "vault"},
	}
	for _, c := range cases {
		rec := h.serveToken(t, handToken(t, hdrHS256, c.payload, h.secret), c.clientRole)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.name, rec.Code)
			continue
		}
		if got := <-seen; got != c.want {
			t.Errorf("%s: upstream X-Role %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRetryKeepsUpstreamRole: a stream cut mid-transfer resumes on another
// pod with a request retryTransport builds afresh, copying only the headers
// it lists. Without X-Role among them Vault's read resumes as anyone's, and
// the seeder may serve the rest from the very copy being checked.
func TestRetryKeepsUpstreamRole(t *testing.T) {
	const size, cut = 1000, 100
	seen := make(chan string, 2)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Role")
		if r.Header.Get("Range") == "" {
			// The first pod dies after cut bytes.
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, cut))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", cut, size-1, size))
		w.Header().Set("Content-Length", strconv.Itoa(size-cut))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, size-cut))
	})
	h.web.pr.maxRetries = 1
	tok := handToken(t, hdrHS256, map[string]any{"role": "vault", "exp": time.Now().Add(time.Hour).Unix()}, h.secret)
	rec := h.serveToken(t, tok, "")
	if rec.Body.Len() != size {
		t.Fatalf("client got %d bytes, want %d: the stream did not resume", rec.Body.Len(), size)
	}
	for _, req := range []string{"first", "retry"} {
		if got := <-seen; got != "vault" {
			t.Errorf("%s request: upstream X-Role %q, want vault", req, got)
		}
	}
}
