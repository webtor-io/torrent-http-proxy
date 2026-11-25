package services

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
)

// TestRetryResumesTheSamePath: a resume asks the pod for the request-target
// the first attempt sent, byte for byte. Until 2026-11 it was rebuilt from
// the decoded path, so a file name with '#' or '?' lost its tail (the seeder
// answered 404 file not found and the resume went on to load the torrent on
// another pod) and one with '%' did not parse or named another file.
func TestRetryResumesTheSamePath(t *testing.T) {
	for _, c := range []struct{ name, path, rawPath string }{
		{"hash", "/dir/S02 - 2008#10#27.mkv", ""},
		{"question mark", "/dir/what?.mkv", ""},
		{"percent", "/dir/100%.mkv", ""},
		{"percent and hex", "/dir/100%41.mkv", ""},
		{"spaces", "/dir 2/with space.mkv", ""},
		{"non-ASCII", "/Папка/Привет, мир ü.mkv", ""},
		{"all of them", "/a #1/b?c 50% ф#.mkv", ""},
		{"escaped slash", "/dir/a/b.mkv", "/dir/a%2Fb.mkv"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRetryRig(t, []string{podA, podB})
			var mu sync.Mutex
			var targets []string
			h := seederHandler(g.pods[podA], false, nil)
			g.serve(podA, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				targets = append(targets, r.RequestURI)
				mu.Unlock()
				h(w, r)
			})
			u := &url.URL{Scheme: "http", Host: podA + ":8080", Path: c.path, RawPath: c.rawPath, RawQuery: "token=t&download"}
			req := WithRetryContext(&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}, &RetryContext{
				Src:               &Source{Type: "default", InfoHash: retryHash},
				SvcLoc:            g.sl,
				Cfg:               &g.cfg,
				Transport:         g.tr,
				ExternalTransport: g.tr,
				MaxRetries:        3,
			})
			resp, err := (&retryTransport{&redirectFollowingTransport{g.tr, g.tr}}).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			wantWhole(t, got, err)
			want := u.RequestURI()
			mu.Lock()
			defer mu.Unlock()
			if len(targets) != 2 || targets[0] != want || targets[1] != want {
				t.Fatalf("podA was asked for %q, want %q twice (first attempt, resume)", targets, want)
			}
		})
	}
}

// TestRetryResumesTheClientsPath: the same through the whole chain, from the
// client's request line through Web's handler (which swaps the URL's Path,
// not its RawPath) and the ReverseProxy's director.
func TestRetryResumesTheClientsPath(t *testing.T) {
	out := logrus.StandardLogger().Out
	logrus.SetOutput(io.Discard)
	t.Cleanup(func() { logrus.SetOutput(out) })
	const file = "/a%20%231/b%3Fc%2050%25%20%D1%84%23.mkv"
	targets := make(chan string, 2)
	seeder := seederHandler(&retryPod{}, false, nil)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		targets <- r.RequestURI
		seeder(w, r)
	})
	h.web.pr.maxRetries = 1
	status, body := h.getPath(t, "/"+harnessHash+file, jwt.MapClaims{}, true)
	wantWhole(t, body, nil)
	first, retry := <-targets, <-targets
	if status != http.StatusOK || !strings.HasPrefix(first, file+"?") || retry != first {
		t.Fatalf("got %d; upstream was asked for %q, then %q; want %s?… twice", status, first, retry, file)
	}
}
