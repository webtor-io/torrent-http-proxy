package services

import (
	"strings"
	"testing"

	"github.com/dgrijalva/jwt-go"
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
