package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

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
	(*h.web.parser.configs)["vod"] = &ServiceConfig{Name: harnessCallerSvc, EndpointsProvider: Environment}
	t.Setenv("NGINX_VOD_SERVICE_HOST", os.Getenv("THP_THROTTLE_TEST_SERVICE_HOST"))
	t.Setenv("NGINX_VOD_SERVICE_PORT", os.Getenv("THP_THROTTLE_TEST_SERVICE_PORT"))
	exp := time.Now().Add(time.Hour).Unix()
	withUid, noUid := jwt.MapClaims{"uid": harnessUid, "exp": exp}, jwt.MapClaims{"exp": exp}
	const segment = "~vod/hls/0a/s-1-v1-a1.ts"
	for _, c := range []struct {
		name, suffix, clientUid, want string
		claims                        jwt.MapClaims
	}{
		{"seeder, token uid", "", "", harnessUid, withUid},
		{"seeder, token uid, client evil", "", "evil", harnessUid, withUid},
		{"seeder, no claim, client evil", "", "evil", "", noUid},
		{"vod edge, token uid, client evil", segment, "evil", harnessUid, withUid},
		{"vod edge, no claim, client evil", segment, "evil", "", noUid},
	} {
		r := h.request(t, c.claims, true, "")
		r.URL.Path += c.suffix
		if c.clientUid != "" {
			r.Header.Set("X-Uid", c.clientUid)
		}
		h.serve(t, httptest.NewRecorder(), r)
		if got := <-seen; got != c.want {
			t.Errorf("%s: upstream X-Uid %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRetryKeepsUpstreamUid: a stream cut mid-transfer resumes with a request
// retryTransport builds afresh, copying only the headers it lists. Without
// X-Uid among them a private torrent's read resumes under no account.
func TestRetryKeepsUpstreamUid(t *testing.T) {
	const size, cut = 1000, 100
	seen := make(chan string, 2)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Uid")
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
	tok := handToken(t, hdrHS256, map[string]any{"uid": harnessUid, "exp": time.Now().Add(time.Hour).Unix()}, h.secret)
	rec := h.serveToken(t, tok, "")
	if rec.Body.Len() != size {
		t.Fatalf("client got %d bytes, want %d: the stream did not resume", rec.Body.Len(), size)
	}
	for _, req := range []string{"first", "retry"} {
		if got := <-seen; got != harnessUid {
			t.Errorf("%s request: upstream X-Uid %q, want %q", req, got, harnessUid)
		}
	}
}
