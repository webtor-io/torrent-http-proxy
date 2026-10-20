package services

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// After nginx-vod's own read of a source was answered 404, the same session's
// segment request to nginx-vod is answered 404 by thp and never proxied.
// Any other 404, or nginx-vod's read of another session, changes nothing.
func TestMissingSourceAnswered404(t *testing.T) {
	var hits atomic.Int32
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Has("found") {
			w.WriteHeader(http.StatusPartialContent)
			return
		}
		http.NotFound(w, r)
	})
	// The vod edge is the harness's caller service, on the same upstream;
	// so is external-proxy, for /ext/<url> sources.
	(*h.web.parser.configs)["vod"] = &ServiceConfig{Name: harnessCallerSvc, EndpointsProvider: Environment}
	(*h.web.parser.configs)["ext"] = &ServiceConfig{Name: "external-proxy", EndpointsProvider: Environment}
	for _, svc := range []string{"NGINX_VOD", "EXTERNAL_PROXY"} {
		t.Setenv(svc+"_SERVICE_HOST", os.Getenv("THP_THROTTLE_TEST_SERVICE_HOST"))
		t.Setenv(svc+"_SERVICE_PORT", os.Getenv("THP_THROTTLE_TEST_SERVICE_PORT"))
	}

	const segment = "~vod/hls/0a/s-1-v1-a1.ts"
	req := func(session string, external bool, suffix, query string) *http.Request {
		r := h.request(t, tierClaims("t-missing", session, "2M"), external, query)
		r.URL.Path += suffix
		return r
	}
	// replace swaps part of r's path: another file, torrent or source type.
	replace := func(r *http.Request, old, new string) *http.Request {
		r.URL.Path = strings.Replace(r.URL.Path, old, new, 1)
		return r
	}
	try := func(name string, r *http.Request, wantStatus int, wantProxied bool) {
		t.Helper()
		before := hits.Load()
		rec := httptest.NewRecorder()
		h.serve(t, rec, r)
		if rec.Code != wantStatus {
			t.Errorf("%s: got %d, want %d", name, rec.Code, wantStatus)
		}
		if proxied := hits.Load() > before; proxied != wantProxied {
			t.Errorf("%s: proxied %v, want %v", name, proxied, wantProxied)
		}
	}

	try("nginx-vod's read", req("s-a", false, "", ""), 404, true)
	try("segment after it", req("s-a", true, segment, ""), 404, false)
	try("another session's segment", req("s-b", true, segment, ""), 404, true)
	// Every part of the key counts: the same session's segment of another
	// file or torrent, and its request for the file to another service.
	try("segment of another file", replace(req("s-a", true, segment, ""), "Sintel.mkv", "Other.mkv"), 404, true)
	try("segment of another torrent", replace(req("s-a", true, segment, ""), "08ada5a7", "18ada5a7"), 404, true)
	try("the file from the seeder", req("s-a", true, "", "&found"), 206, true)

	// rest-api's ?done probe on the viewer's token comes from outside: its
	// 404 says "not cached", not "not there", and is not kept at all.
	kept := h.web.missing.Len()
	try("external read", req("s-c", true, "", "&done=true"), 404, true)
	if h.web.missing.Len() != kept {
		t.Errorf("an external 404 was kept")
	}
	try("segment after an external 404", req("s-c", true, segment, ""), 404, true)

	// external-proxy's 404 for an /ext/<url> source can be the remote's
	// passing state, not a file the torrent lacks: not kept either.
	const ext = "/08ada5a7a6183aae1e09d831df6748d566095a10/"
	try("nginx-vod's read of an /ext source", replace(req("s-f", false, "", ""), ext, "/ext/"), 404, true)
	if h.web.missing.Len() != kept {
		t.Errorf("an /ext source's 404 was kept")
	}
	try("segment after an /ext 404", replace(req("s-f", true, segment, ""), ext, "/ext/"), 404, true)

	// nginx-vod's request through a service is not its read of the source.
	try("nginx-vod's request through vod", req("s-d", false, segment, ""), 404, true)
	try("segment after a service's 404", req("s-d", true, segment, ""), 404, true)

	try("nginx-vod's read that found it", req("s-e", false, "", "&found"), 206, true)
	try("segment after a 206", req("s-e", true, segment, ""), 404, true)

	// A token without a session is shared by whoever holds it.
	try("nginx-vod's read without a session", req("", false, "", ""), 404, true)
	try("segment without a session", req("", true, segment, ""), 404, true)
}
