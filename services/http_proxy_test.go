package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// followed sends a GET for url, with thp's X-Info-Hash and a Range, through
// redirectFollowingTransport and returns the final status and body.
func followed(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Info-Hash", "08ada5a7a6183aae1e09d831df6748d566095a10")
	req.Header.Set("Range", "bytes=2-")
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&redirectFollowingTransport{tr, tr}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// The seeder's ServeMux answers "Name//file" (a torrent's empty path
// component) with a 307 to the cleaned path, relative. thp follows it to the
// same pod with the headers it set, and gets the file: not "unsupported
// protocol scheme" (502), nor, without X-Info-Hash, the pod's torrent index.
func TestRedirectFollowsRelativeLocation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Info-Hash") == "" {
			_, _ = io.WriteString(w, "<h1>Index</h1>") // as the seeder does
			return
		}
		if r.URL.Path != "/Name/file" || r.Header.Get("Range") != "bytes=2-" {
			http.Error(w, r.URL.Path+" "+r.Header.Get("Range"), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "le")
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	direct, err := http.NewRequest(http.MethodGet, up.URL+"/Name//file?token=t", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultTransport.RoundTrip(direct)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusTemporaryRedirect || loc != "/Name/file?token=t" {
		t.Fatalf("upstream answers %d %q, not a relative 307: the test proves nothing", resp.StatusCode, loc)
	}

	if code, body := followed(t, up.URL+"/Name//file?token=t"); code != http.StatusPartialContent || body != "le" {
		t.Fatalf("got %d %q, want 206 \"le\"", code, body)
	}
}

// A redirect to another host (Vault's presigned S3 URL) gets the Range and
// nothing else thp set: the viewer's token is not the target's business. A
// relative redirect from there stays on that host.
func TestRedirectToAnotherHostKeepsOnlyRange(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/object" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path != "/final" || r.Header.Get("X-Info-Hash") != "" || r.Header.Get("Range") != "bytes=2-" {
			http.Error(w, r.URL.Path+": X-Info-Hash "+r.Header.Get("X-Info-Hash")+", Range "+r.Header.Get("Range"), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "le")
	}))
	t.Cleanup(target.Close)
	up := httptest.NewServer(http.RedirectHandler(target.URL+"/object", http.StatusFound))
	t.Cleanup(up.Close)

	if code, body := followed(t, up.URL+"/file"); code != http.StatusPartialContent || body != "le" {
		t.Fatalf("got %d %q, want 206 \"le\"", code, body)
	}
}
