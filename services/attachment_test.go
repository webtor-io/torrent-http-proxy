package services

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// A type a browser opens as a page gets an attachment, whatever the client
// asked; anything else only on ?download with no Content-Disposition; an
// attachment from upstream stays as it is.
func TestSetAttachment(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		ct       string // "-" for no Content-Type at all
		ct2      string // a second Content-Type header
		empty    bool   // Content-Length: 0 (else unknown)
		cd       string
		file     string
		download bool
		external bool   // the edge is external-proxy
		want     string // "" for no Content-Disposition
	}{
		{name: "html", status: 200, ct: "text/html; charset=utf-8", file: "/Pack/index.html", want: "attachment; filename=index.html"},
		{name: "svg, a range", status: 206, ct: "image/svg+xml", file: "/Pack/logo.svg", want: "attachment; filename=logo.svg"},
		{name: "xhtml", status: 200, ct: "application/xhtml+xml", file: "/a.xhtml", want: "attachment; filename=a.xhtml"},
		{name: "xml", status: 200, ct: "text/xml; charset=utf-8", file: "/a.xml", want: "attachment; filename=a.xml"},
		{name: "xsl: Blink and WebKit open it as XML", status: 200, ct: "TEXT/XSL; charset=utf-8", file: "/ext/a", want: "attachment; filename=a"},
		{name: "no type: sniffed", status: 200, ct: "-", file: "/Pack/README", want: "attachment; filename=README"},
		{name: "unknown/unknown: sniffed", status: 200, ct: "unknown/unknown", file: "/x", want: "attachment; filename=x"},
		{name: "inline html is replaced", status: 200, ct: "text/html", cd: `inline; filename="a.html"`, file: "/a.html", want: "attachment; filename=a.html"},
		{name: "name with a space is quoted", status: 200, ct: "text/html", file: "/a b.html", want: `attachment; filename="a b.html"`},
		{name: "no file name", status: 200, ct: "text/html", file: "/", want: "attachment"},
		{name: "mp4 stays inline", status: 206, ct: "video/mp4", file: "/Sintel/Sintel.mp4"},
		{name: "playlist stays inline", status: 200, ct: "application/vnd.apple.mpegurl", file: "/session/abc/index.m3u8"},
		{name: "vtt stays inline", status: 200, ct: "text/vtt; charset=utf-8", file: "/sub.vtt"},
		{name: "image stays inline", status: 200, ct: "image/jpeg", file: "/cover.jpg"},
		{name: "download without one: non-ASCII name", status: 206, ct: "video/mp4", file: "/Папка/Фильм 1.mp4", download: true,
			want: "attachment; filename*=utf-8''%D0%A4%D0%B8%D0%BB%D1%8C%D0%BC%201.mp4"},
		{name: "download, upstream inline kept", status: 200, ct: "video/mp4", cd: "inline", file: "/a.mp4", download: true, want: "inline"},
		{name: "upstream attachment kept", status: 200, ct: "text/html", cd: `attachment; filename="a"b.html"`, file: "/a.html", download: true,
			want: `attachment; filename="a"b.html"`},
		{name: "two types: either may be taken", status: 200, ct: "text/plain", ct2: "text/html", file: "/a.txt", want: "attachment; filename=a.txt"},
		{name: "comma after params: a browser takes the last", status: 200, ct: "text/plain;charset=utf-8,text/html", file: "/a.txt", want: "attachment; filename=a.txt"},
		{name: "comma after params, image first", status: 200, ct: "image/png;a=b,text/html", file: "/a.png", want: "attachment; filename=a.png"},
		{name: "external: an image too", status: 200, ct: "image/png", file: "/poster.png", external: true, want: "attachment; filename=poster.png"},
		{name: "external: a pdf", status: 200, ct: "application/pdf", file: "/a.pdf", external: true, want: "attachment; filename=a.pdf"},
		{name: "external without a body untouched", status: 404, ct: "-", empty: true, file: "/a.png", external: true},
		{name: "404 page is a page too", status: 404, ct: "text/html", file: "/a.html", download: true, want: "attachment; filename=a.html"},
		{name: "404 without a body untouched", status: 404, ct: "-", empty: true, file: "/a.mkv", download: true},
		{name: "404 text untouched", status: 404, ct: "text/plain; charset=utf-8", file: "/a.mkv", download: true},
		{name: "304 untouched", status: 304, ct: "-", empty: true, file: "/a.html"},
		{name: "empty download still saved", status: 200, ct: "-", empty: true, file: "/a.bin", download: true, want: "attachment; filename=a.bin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := withAttachmentRequest(httptest.NewRequest(http.MethodGet, "http://seeder/", nil), c.file, c.download, c.external)
			resp := &http.Response{StatusCode: c.status, Header: http.Header{}, Request: req, ContentLength: -1}
			if c.empty {
				resp.ContentLength = 0
			}
			if c.ct != "-" {
				resp.Header.Set("Content-Type", c.ct)
			}
			if c.ct2 != "" {
				resp.Header.Add("Content-Type", c.ct2)
			}
			if c.cd != "" {
				resp.Header.Set("Content-Disposition", c.cd)
			}
			setAttachment(resp)
			if got := resp.Header.Get("Content-Disposition"); got != c.want {
				t.Fatalf("Content-Disposition %q, want %q", got, c.want)
			}
		})
	}
}

// The non-ASCII name reads back as it was: the filename* above is what a
// browser decodes, not a mangled ASCII stand-in.
func TestSetAttachmentNameRoundTrips(t *testing.T) {
	req := withAttachmentRequest(httptest.NewRequest(http.MethodGet, "http://seeder/", nil), "/Папка/Фильм 1.mp4", true, false)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Request: req}
	setAttachment(resp)
	d, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || d != "attachment" || params["filename"] != "Фильм 1.mp4" {
		t.Fatalf("parsed %q %v %v", d, params, err)
	}
}

// A file Vault has: the seeder answers ?download with a 302 (its attachment
// on it, which thp drops with the hop), and the file comes from another host
// (s3-cache) without a Content-Disposition and without the client's query.
// The client still gets an attachment named after what it asked for: the
// flag rides the request's context through redirectFollowingTransport.follow.
// Without ?download the same mp4 stays inline.
func TestProxyHTTPDownloadFromVaultIsAttachment(t *testing.T) {
	const file = "/Папка/Фильм.mp4"
	var finalQuery string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "mp4")
	}))
	t.Cleanup(cache.Close)
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["download"]; ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="Фильм.mp4"`)
		}
		http.Redirect(w, r, cache.URL+"/c9f100602e7df12cb657843400c9a77fc3cc8956", http.StatusFound)
	})
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": "free", "sessionID": "s-cd"}).SignedString([]byte(h.secret))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query string
		want  string
	}{
		{"&download=true", "attachment; filename*=utf-8''%D0%A4%D0%B8%D0%BB%D1%8C%D0%BC.mp4"},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodGet,
			"/08ada5a7a6183aae1e09d831df6748d566095a10"+(&url.URL{Path: file}).EscapedPath()+"?token="+tok+"&api-key=k"+c.query, nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		src, err := h.web.parser.Parse(r.URL)
		if err != nil {
			t.Fatal(err)
		}
		r.URL.Path = src.Path // what Serve does for a mod-less source
		rec := httptest.NewRecorder()
		logger, _ := logtest.NewNullLogger()
		h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "mp4" {
			t.Fatalf("query %q: %d %q, want the cache's 206 \"mp4\": the redirect was not followed", c.query, rec.Code, rec.Body.String())
		}
		if finalQuery != "" {
			t.Fatalf("the cache got query %q: the test proves nothing about the context", finalQuery)
		}
		if got := rec.Header().Get("Content-Disposition"); got != c.want {
			t.Errorf("query %q: Content-Disposition %q, want %q", c.query, got, c.want)
		}
	}
}

// /ext/ is external-proxy, whose response type the URL's author picks: an
// attachment whatever the type, an image included (<img> ignores it). The
// same image from a torrent stays inline.
func TestProxyHTTPExtIsAlwaysAttachment(t *testing.T) {
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "png")
	})
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": "free", "sessionID": "s-ext"}).SignedString([]byte(h.secret))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		want string
	}{
		{"/ext/aHR0cHM6Ly9leGFtcGxlLmNvbS9wLnBuZw/poster.png", "attachment; filename=poster.png"},
		{"/08ada5a7a6183aae1e09d831df6748d566095a10/poster.png", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, c.path+"?token="+tok+"&api-key=k", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		src, err := h.web.parser.Parse(r.URL)
		if err != nil {
			t.Fatal(err)
		}
		r.URL.Path = src.Path
		rec := httptest.NewRecorder()
		logger, _ := logtest.NewNullLogger()
		h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))
		if rec.Code != http.StatusOK || rec.Body.String() != "png" {
			t.Fatalf("%s: %d %q, want the upstream's 200 \"png\"", c.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Disposition"); got != c.want {
			t.Errorf("%s: Content-Disposition %q, want %q", c.path, got, c.want)
		}
	}
}
