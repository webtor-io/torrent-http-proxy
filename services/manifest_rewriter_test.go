package services

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

const (
	primaryJWT = "PRIMARY_TOKEN"
	graceJWT   = "GRACE_TOKEN"
)

// claimsWithGrace returns a MapClaims that carries one grace/manifest rule.
func claimsWithGrace(durationSec int) jwt.MapClaims {
	return jwt.MapClaims{
		"rate": "5M",
		"role": "free",
		"rules": []interface{}{
			map[string]interface{}{
				"kind":         "grace",
				"scope":        "manifest",
				"duration_sec": float64(durationSec),
				"token":        graceJWT,
			},
		},
	}
}

func TestRewriteManifest_NoRule_PassesThrough(t *testing.T) {
	body := []byte("#EXTM3U\n#EXTINF:6.0,\nv0-0.ts?token=" + primaryJWT + "\n")
	out := RewriteManifest(body, jwt.MapClaims{"rate": "5M"}, primaryJWT)
	if string(out) != string(body) {
		t.Fatalf("expected pass-through, got: %s", out)
	}
}

func TestRewriteManifest_NoPrimary_PassesThrough(t *testing.T) {
	body := []byte("#EXTM3U\n#EXTINF:6.0,\nv0-0.ts\n")
	out := RewriteManifest(body, claimsWithGrace(1200), "")
	if string(out) != string(body) {
		t.Fatalf("expected pass-through, got: %s", out)
	}
}

func TestRewriteManifest_FreshOffset_FirstSegmentsGetGrace(t *testing.T) {
	// 4 × 6s segments, grace 12s → first 2 segments swap (start=0, start=6)
	body := []byte(strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-SESSION-OFFSET:0",
		"#EXT-X-TARGETDURATION:6",
		"#EXTINF:6.0,",
		"v0-0.ts?token=" + primaryJWT + "&api-key=K",
		"#EXTINF:6.0,",
		"v0-1.ts?token=" + primaryJWT + "&api-key=K",
		"#EXTINF:6.0,",
		"v0-2.ts?token=" + primaryJWT + "&api-key=K",
		"#EXTINF:6.0,",
		"v0-3.ts?token=" + primaryJWT + "&api-key=K",
		"",
	}, "\n"))
	out := RewriteManifest(body, claimsWithGrace(12), primaryJWT)
	got := string(out)

	if strings.Contains(got, "#EXT-X-SESSION-OFFSET:") {
		t.Errorf("session offset tag should be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-0.ts?token="+graceJWT) {
		t.Errorf("seg 0 should have grace token, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-1.ts?token="+graceJWT) {
		t.Errorf("seg 1 should have grace token, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-2.ts?token="+primaryJWT) {
		t.Errorf("seg 2 should keep primary token, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-3.ts?token="+primaryJWT) {
		t.Errorf("seg 3 should keep primary token, got:\n%s", got)
	}
}

func TestRewriteManifest_OffsetPastGrace_NoSwap(t *testing.T) {
	body := []byte(strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-SESSION-OFFSET:1500",
		"#EXTINF:6.0,",
		"v0-250.ts?token=" + primaryJWT,
		"",
	}, "\n"))
	out := RewriteManifest(body, claimsWithGrace(1200), primaryJWT)
	got := string(out)

	if strings.Contains(got, graceJWT) {
		t.Errorf("no segment should swap when session offset already past grace, got:\n%s", got)
	}
	if strings.Contains(got, "#EXT-X-SESSION-OFFSET:") {
		t.Errorf("offset tag should be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-250.ts?token="+primaryJWT) {
		t.Errorf("primary token should remain, got:\n%s", got)
	}
}

func TestRewriteManifest_OffsetMidGrace_PartialSwap(t *testing.T) {
	// offset=900, grace=1200 → 300s of grace remain → 50 segments at 6s each
	body := []byte(strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-SESSION-OFFSET:900",
		"#EXTINF:6.0,",
		"v0-150.ts?token=" + primaryJWT, // start=900, in grace
		"#EXTINF:6.0,",
		"v0-151.ts?token=" + primaryJWT, // start=906, in grace
		"",
	}, "\n"))
	out := RewriteManifest(body, claimsWithGrace(1200), primaryJWT)
	got := string(out)
	if !strings.Contains(got, "v0-150.ts?token="+graceJWT) {
		t.Errorf("seg 150 should swap, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-151.ts?token="+graceJWT) {
		t.Errorf("seg 151 should swap, got:\n%s", got)
	}
}

func TestRewriteManifest_NoOffsetTag_DefaultsToZero(t *testing.T) {
	body := []byte(strings.Join([]string{
		"#EXTM3U",
		"#EXTINF:6.0,",
		"v0-0.ts?token=" + primaryJWT,
		"",
	}, "\n"))
	out := RewriteManifest(body, claimsWithGrace(1200), primaryJWT)
	if !strings.Contains(string(out), "v0-0.ts?token="+graceJWT) {
		t.Errorf("missing offset tag should default to 0 and apply grace, got:\n%s", string(out))
	}
}

func TestRewriteManifest_MasterPlaylist_NoOp(t *testing.T) {
	// Master playlist has no #EXTINF / segment lines.
	body := []byte(strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-SESSION-OFFSET:0",
		"#EXT-X-STREAM-INF:BANDWIDTH=5000000",
		"v0-720.m3u8?token=" + primaryJWT,
		"",
	}, "\n"))
	out := RewriteManifest(body, claimsWithGrace(1200), primaryJWT)
	got := string(out)
	if strings.Contains(got, graceJWT) {
		t.Errorf("master playlist must not be rewritten (no EXTINF), got:\n%s", got)
	}
	if !strings.Contains(got, "v0-720.m3u8?token="+primaryJWT) {
		t.Errorf("master playlist variant URL must stay primary, got:\n%s", got)
	}
}

func TestExtractRules(t *testing.T) {
	mc := claimsWithGrace(1200)
	rules := ExtractRules(mc)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Kind != "grace" || r.Scope != "manifest" || r.DurationSec != 1200 || r.Token != graceJWT {
		t.Errorf("unexpected rule: %+v", r)
	}
}

func TestExtractRules_Missing(t *testing.T) {
	if rules := ExtractRules(jwt.MapClaims{"rate": "5M"}); rules != nil {
		t.Errorf("want nil, got %+v", rules)
	}
}

func TestParseSessionOffset(t *testing.T) {
	cases := []struct {
		body string
		want float64
	}{
		{"#EXTM3U\n#EXT-X-SESSION-OFFSET:0\n", 0},
		{"#EXTM3U\n#EXT-X-SESSION-OFFSET:1500\n", 1500},
		{"#EXTM3U\n", 0},
		{"#EXTM3U\n#EXT-X-SESSION-OFFSET:abc\n", 0},
	}
	for _, c := range cases {
		got := parseSessionOffset([]byte(c.body))
		if got != c.want {
			t.Errorf("body=%q want=%v got=%v", c.body, c.want, got)
		}
	}
}

// fMP4 playlists, as content-transcoder serves them for a passthrough
// session (its docs/session-transcoding.md, "Passthrough output"): the hls
// muxer's own playlist -- FFmpeg 8.1.2, -hls_segment_type fmp4 -hls_flags
// temp_file -hls_fmp4_init_filename <prefix>-init-<gen>.mp4 -- after
// PlaylistForStream (#EXT-X-SESSION-OFFSET and #EXT-X-START after #EXTM3U,
// #EXT-X-ENDLIST held back while the run produces), with the client's query
// appended to every reference, the URI in #EXT-X-MAP included. The query is
// in the order browsers send it, api-key first: the token is matched as
// &token=. Two cases carry what that binary wrote in content-transcoder's
// end-to-end runs, each case says which; the others are built in the same
// layout.
//
// The rule these pin: only a URI line (the one after #EXTINF) is a segment
// whose token follows the window. A URI inside a tag -- the init in
// #EXT-X-MAP, a rendition in #EXT-X-MEDIA -- stays on the primary token
// with the rest of the line: every line that starts with '#' passes
// through byte for byte.
const (
	fmp4Primary = "?api-key=K&token=" + primaryJWT
	fmp4Grace   = "?api-key=K&token=" + graceJWT
)

func m3u8(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// firstDiff names the first line where got and want part.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d: got %q, want %q", i+1, gl, wl)
		}
	}
	return "no difference"
}

func TestRewriteManifest_FMP4(t *testing.T) {
	const p, g = fmp4Primary, fmp4Grace
	cases := []struct {
		name  string
		grace int
		in    string
		want  string
	}{
		{
			// v0-2160.m3u8.ffmpeg of an end-to-end run from the start (4 s
			// GOPs, generation 216d035412fb7bb8): the tags, #EXTINF and URI
			// lines as FFmpeg wrote them, with PlaylistForStream's two tags,
			// no #EXT-X-ENDLIST and the query added. Grace 8: segments
			// starting at 0 and 4 are in the window, the one at 8 is not.
			name:  "video from the start",
			grace: 8,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:0.000",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-216d035412fb7bb8.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-0.m4s"+p,
				"#EXTINF:4.000000,",
				"v0-2160-1.m4s"+p,
				"#EXTINF:4.059000,",
				"v0-2160-2.m4s"+p,
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-216d035412fb7bb8.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-0.m4s"+g,
				"#EXTINF:4.000000,",
				"v0-2160-1.m4s"+g,
				"#EXTINF:4.059000,",
				"v0-2160-2.m4s"+p,
			),
		},
		{
			// A run after a seek: the offset is its real start, a keyframe
			// before the quantized point; 10 s GOPs at 23.976 fps (built,
			// not copied from a run). The
			// window (1200) ends inside the playlist: .m4s segments before
			// it on grace, after it on primary, the init on primary.
			name:  "video after a seek, window ends inside",
			grace: 1200,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:1186.937",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:10",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:10.010000,",
				"v0-2160-0.m4s"+p, // 1186.937
				"#EXTINF:10.010000,",
				"v0-2160-1.m4s"+p, // 1196.947
				"#EXTINF:10.010000,",
				"v0-2160-2.m4s"+p, // 1206.957
				"#EXTINF:8.341667,",
				"v0-2160-3.m4s"+p, // 1216.967
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:10",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:10.010000,",
				"v0-2160-0.m4s"+g,
				"#EXTINF:10.010000,",
				"v0-2160-1.m4s"+g,
				"#EXTINF:10.010000,",
				"v0-2160-2.m4s"+p,
				"#EXTINF:8.341667,",
				"v0-2160-3.m4s"+p,
			),
		},
		{
			// a0.m3u8.ffmpeg of another end-to-end run, one started at 30 s
			// (generation f2358d9841203756), added to as above: audio is
			// fMP4 too and cut on its own clock. The offset is not that
			// run's: 1190 puts the window's end inside the playlist.
			name:  "audio rendition",
			grace: 1200,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:1190.000",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="a0-init-f2358d9841203756.mp4`+p+`"`,
				"#EXTINF:4.010667,",
				"a0-0.m4s"+p, // 1190
				"#EXTINF:3.989333,",
				"a0-1.m4s"+p, // 1194.010667
				"#EXTINF:4.010667,",
				"a0-2.m4s"+p, // 1198
				"#EXTINF:0.106667,",
				"a0-3.m4s"+p, // 1202.010667
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="a0-init-f2358d9841203756.mp4`+p+`"`,
				"#EXTINF:4.010667,",
				"a0-0.m4s"+g,
				"#EXTINF:3.989333,",
				"a0-1.m4s"+g,
				"#EXTINF:4.010667,",
				"a0-2.m4s"+g,
				"#EXTINF:0.106667,",
				"a0-3.m4s"+p,
			),
		},
		{
			// Past the window: nothing swaps, the offset tag goes. The fMP4
			// form of TestRewriteManifest_OffsetPastGrace_NoSwap; it does not
			// pin RewriteManifest's early return for this, which the walk
			// matches (without it the package stays green).
			name:  "video past the window",
			grace: 1200,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:1500.000",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:10",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:10.010000,",
				"v0-2160-0.m4s"+p,
				"#EXTINF:10.010000,",
				"v0-2160-1.m4s"+p,
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:10",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:10.010000,",
				"v0-2160-0.m4s"+p,
				"#EXTINF:10.010000,",
				"v0-2160-1.m4s"+p,
			),
		},
		{
			// A second init after a discontinuity (RFC 8216 4.3.2.5: a MAP
			// applies to the segments after it until the next). FFmpeg's
			// hls muxer writes #EXT-X-MAP once, before the first segment
			// (hlsenc.c hls_window), so content-transcoder's playlists do
			// not have this; the shape is the spec's. The second MAP stays
			// on primary with segments on grace either side of it, and
			// movie time runs on across the discontinuity: the segment at
			// 12 is past the window, not at 4 of a new count.
			name:  "discontinuity and a second MAP",
			grace: 12,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:0.000",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-0.m4s"+p,
				"#EXTINF:4.000000,",
				"v0-2160-1.m4s"+p,
				"#EXT-X-DISCONTINUITY",
				`#EXT-X-MAP:URI="v0-2160-init-5f00e1d2c3b4a596.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-2.m4s"+p,
				"#EXTINF:4.000000,",
				"v0-2160-3.m4s"+p,
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-0.m4s"+g,
				"#EXTINF:4.000000,",
				"v0-2160-1.m4s"+g,
				"#EXT-X-DISCONTINUITY",
				`#EXT-X-MAP:URI="v0-2160-init-5f00e1d2c3b4a596.mp4`+p+`"`,
				"#EXTINF:4.000000,",
				"v0-2160-2.m4s"+g,
				"#EXTINF:4.000000,",
				"v0-2160-3.m4s"+p,
			),
		},
		{
			// FFmpeg 8.1.2's -hls_flags single_file output (not a flag
			// content-transcoder uses): the init and every segment are
			// byte ranges of one file. BYTERANGE on the MAP and the
			// #EXT-X-BYTERANGE between #EXTINF and its URI pass through,
			// and the tag in between does not part #EXTINF from its URI.
			name:  "byte ranges",
			grace: 8,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:0.000",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-180.m4s`+p+`",BYTERANGE="3241@0"`,
				"#EXTINF:4.000000,",
				"#EXT-X-BYTERANGE:81787@3241",
				"v0-180.m4s"+p,
				"#EXTINF:4.000000,",
				"#EXT-X-BYTERANGE:87512@85028",
				"v0-180.m4s"+p,
				"#EXTINF:4.080000,",
				"#EXT-X-BYTERANGE:83739@172540",
				"v0-180.m4s"+p,
			),
			want: m3u8(
				"#EXTM3U",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:4",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-180.m4s`+p+`",BYTERANGE="3241@0"`,
				"#EXTINF:4.000000,",
				"#EXT-X-BYTERANGE:81787@3241",
				"v0-180.m4s"+g,
				"#EXTINF:4.000000,",
				"#EXT-X-BYTERANGE:87512@85028",
				"v0-180.m4s"+g,
				"#EXTINF:4.080000,",
				"#EXT-X-BYTERANGE:83739@172540",
				"v0-180.m4s"+p,
			),
		},
		{
			// A passthrough session's master (passthroughMasterPlaylist,
			// served with the offset): the renditions' URIs are inside
			// #EXT-X-MEDIA and stay on primary, as does the variant (no
			// #EXTINF before it).
			name:  "passthrough master",
			grace: 1200,
			in: m3u8(
				"#EXTM3U",
				"#EXT-X-SESSION-OFFSET:0.000",
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="English",AUTOSELECT=YES,DEFAULT=YES,URI="a0.m3u8`+p+`"`,
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="English",URI="s0.m3u8`+p+`"`,
				`#EXT-X-STREAM-INF:BANDWIDTH=48000000,RESOLUTION=3840x2160,CODECS="hvc1.2.4.L150.90,mp4a.40.2",VIDEO-RANGE=PQ,AUDIO="audio",SUBTITLES="subtitles"`,
				"v0-2160.m3u8"+p,
			),
			want: m3u8(
				"#EXTM3U",
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="eng",NAME="English",AUTOSELECT=YES,DEFAULT=YES,URI="a0.m3u8`+p+`"`,
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="eng",NAME="English",URI="s0.m3u8`+p+`"`,
				`#EXT-X-STREAM-INF:BANDWIDTH=48000000,RESOLUTION=3840x2160,CODECS="hvc1.2.4.L150.90,mp4a.40.2",VIDEO-RANGE=PQ,AUDIO="audio",SUBTITLES="subtitles"`,
				"v0-2160.m3u8"+p,
			),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(RewriteManifest([]byte(c.in), claimsWithGrace(c.grace), primaryJWT))
			if got != c.want {
				t.Errorf("%s\ngot:\n%s", firstDiff(got, c.want), got)
			}
		})
	}
}

// TestProxyHTTP_FMP4PlaylistGrace sends a passthrough variant through
// proxyHTTP to an upstream that appends the request's query to every
// reference, the MAP URI included, as content-transcoder does. Two
// upstreams, since thp fronts both kinds:
//
//   - plain, as content-transcoder answers a playlist: uncompressed whatever
//     it is asked, with the Content-Length of what it sent. The rewrite
//     changes the length, so a Content-Length left from the upstream would
//     promise more bytes than the body has: the client reads the whole body
//     and then gets an unexpected EOF. The playlists a passthrough session
//     serves come this way.
//   - gzip, as nginx-vod answers (gzip on for application/vnd.apple.mpegurl):
//     compressed when asked. The client's Accept-Encoding is dropped
//     (HTTPProxy.get), the Transport asks for gzip on its own and undoes it
//     before modifyResponse, taking the upstream's Content-Length away with
//     the encoding.
//
// Either way the rewrite sees plain text and the client gets the rewritten
// playlist uncompressed with its own length.
func TestProxyHTTP_FMP4PlaylistGrace(t *testing.T) {
	in := m3u8(
		"#EXTM3U",
		"#EXT-X-SESSION-OFFSET:1186.937",
		"#EXT-X-START:TIME-OFFSET=0",
		"#EXT-X-VERSION:7",
		"#EXT-X-TARGETDURATION:10",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-PLAYLIST-TYPE:EVENT",
		`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4?Q"`,
		"#EXTINF:10.010000,",
		"v0-2160-0.m4s?Q",
		"#EXTINF:10.010000,",
		"v0-2160-1.m4s?Q",
		"#EXTINF:10.010000,",
		"v0-2160-2.m4s?Q",
	)
	for _, up := range []struct {
		name string
		gzip bool
	}{
		{name: "plain with Content-Length, as content-transcoder"},
		{name: "gzip when asked, as nginx-vod", gzip: true},
	} {
		t.Run(up.name, func(t *testing.T) {
			var (
				mu         sync.Mutex
				upPath     string
				upEncoding string
				upLength   int
				compressed bool
			)
			h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
				body := strings.ReplaceAll(in, "?Q", "?"+r.URL.RawQuery)
				mu.Lock()
				upPath, upEncoding, upLength = r.URL.Path, r.Header.Get("Accept-Encoding"), len(body)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				if up.gzip && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
					var b bytes.Buffer
					zw := gzip.NewWriter(&b)
					_, _ = zw.Write([]byte(body))
					_ = zw.Close()
					mu.Lock()
					compressed = true
					mu.Unlock()
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
					_, _ = w.Write(b.Bytes())
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = io.WriteString(w, body)
			})
			// The transcoder is a mod: .../<file>~hls/session/<id>/<playlist>.
			(*h.web.parser.configs)["hls"] = &ServiceConfig{Name: throttleTestSvc, EndpointsProvider: Environment}

			tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsWithGrace(1200)).SignedString([]byte(h.secret))
			if err != nil {
				t.Fatal(err)
			}
			query := "api-key=k&token=" + tok
			r := httptest.NewRequest(http.MethodGet,
				"/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel/Sintel.mkv~hls/session/abc/v0-2160.m3u8?"+query, nil)
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
			r.Header.Set("Accept-Encoding", "gzip, br")
			src, err := h.web.parser.Parse(r.URL)
			if err != nil || src.Mod == nil {
				t.Fatalf("parse: %v %+v", err, src)
			}
			r.URL.Path = src.Mod.Path // what Serve does for a mod
			rec := httptest.NewRecorder()
			logger, _ := logtest.NewNullLogger()
			h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))

			p, g := "?"+query, "?api-key=k&token="+graceJWT
			want := m3u8(
				"#EXTM3U",
				"#EXT-X-START:TIME-OFFSET=0",
				"#EXT-X-VERSION:7",
				"#EXT-X-TARGETDURATION:10",
				"#EXT-X-MEDIA-SEQUENCE:0",
				"#EXT-X-PLAYLIST-TYPE:EVENT",
				`#EXT-X-MAP:URI="v0-2160-init-0a12b34c56d78e90.mp4`+p+`"`,
				"#EXTINF:10.010000,",
				"v0-2160-0.m4s"+g,
				"#EXTINF:10.010000,",
				"v0-2160-1.m4s"+g,
				"#EXTINF:10.010000,",
				"v0-2160-2.m4s"+p,
			)
			mu.Lock()
			defer mu.Unlock()
			if upPath != "/session/abc/v0-2160.m3u8" {
				t.Fatalf("upstream got %q", upPath)
			}
			// The Transport asks for gzip in both cases: the client's
			// header is gone and it adds its own. Only nginx-vod's kind
			// answers compressed.
			if upEncoding != "gzip" || compressed != up.gzip {
				t.Errorf("upstream saw Accept-Encoding %q, compressed %v: want the Transport's own gzip, not the client's, compressed %v",
					upEncoding, compressed, up.gzip)
			}
			// Otherwise the upstream's Content-Length would pass as the
			// rewritten body's and the check below would prove nothing.
			if upLength == len(want) {
				t.Fatalf("the rewrite keeps the length (%d): the test cannot tell the upstream's Content-Length from its own", upLength)
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" {
				t.Fatalf("status %d, Content-Encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
			}
			if got := rec.Body.String(); got != want {
				t.Errorf("%s\ngot:\n%s", firstDiff(got, want), got)
			}
			if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("Content-Length %q for %d bytes", cl, rec.Body.Len())
			}
		})
	}
}
