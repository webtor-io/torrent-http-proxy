package services

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Tokens here are built by hand, not with the jwt library: the library is
// what is under test, and the shapes that must be refused (alg none, a
// header that lies about the algorithm, padded segments) are ones it will
// not produce.

func seg64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jsonSeg(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return seg64(b)
}

func macSig(h func() hash.Hash, key, input string) string {
	m := hmac.New(h, []byte(key))
	m.Write([]byte(input))
	return seg64(m.Sum(nil))
}

// handToken is header.payload signed with HMAC-SHA256 under key, whatever
// alg the header names.
func handToken(t *testing.T, header, payload any, key string) string {
	t.Helper()
	in := jsonSeg(t, header) + "." + jsonSeg(t, payload)
	return in + "." + macSig(sha256.New, key, in)
}

var (
	hdrHS256 = map[string]any{"alg": "HS256", "typ": "JWT"}
	hdrHS512 = map[string]any{"alg": "HS512", "typ": "JWT"}
	hdrRS256 = map[string]any{"alg": "RS256", "typ": "JWT"}
)

// viewerClaims is the shape web-ui mints for a viewer: bound to the harness
// torrent, with an expiry. It is also a session-stats token.
func viewerClaims(exp any) map[string]any {
	return map[string]any{
		"role":      "paid",
		"rate":      "20M",
		"sessionID": "s-parse",
		"domain":    "example.org",
		"hash":      harnessHash,
		"exp":       exp,
	}
}

func viewerWith(exp any, change func(map[string]any)) map[string]any {
	c := viewerClaims(exp)
	change(c)
	return c
}

// graceClaims is the inner grace token web-ui mints: bound to one torrent,
// no expiry (it is bounded by movie time, not wall time).
func graceClaims(infoHash string) map[string]any {
	return map[string]any{"rate": "20M", "role": "paid", "hash": infoHash, "kind": "grace"}
}

type parseCase struct {
	name    string
	token   string
	apiKey  string         // "" means the right one
	payload map[string]any // the claims an accepted token must come back with
	// get: Claims.Get takes the token. stats: the session-stats stream opens
	// for it, which on top needs hash, sessionID and exp.
	get, stats bool
	// quiet: proxyHTTP logs the refusal at Debug (expired or malformed:
	// stale embeds, not actionable) rather than Warn.
	quiet bool
}

// parseCases are the tokens every parse site sees, signed for secret.
func parseCases(t *testing.T, secret string) []parseCase {
	t.Helper()
	now := time.Now()
	later := now.Add(time.Hour).Unix()
	good := viewerClaims(later)
	goodTok := handToken(t, hdrHS256, good, secret)
	parts := strings.Split(goodTok, ".")

	hs512In := jsonSeg(t, hdrHS512) + "." + jsonSeg(t, good)
	hs512 := hs512In + "." + macSig(sha512.New, secret, hs512In)

	noneIn := jsonSeg(t, map[string]any{"alg": "none", "typ": "JWT"}) + "." + jsonSeg(t, good)

	// An RS256 token under a key of the caller's own.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsIn := jsonSeg(t, hdrRS256) + "." + jsonSeg(t, good)
	digest := sha256.Sum256([]byte(rsIn))
	rsSig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	// Every segment padded, the signature taken over the padded input: a
	// valid HMAC, only the encoding is off.
	pad := func(s string) string { return s + strings.Repeat("=", (4-len(s)%4)%4) }
	padIn := pad(jsonSeg(t, hdrHS256)) + "." + pad(jsonSeg(t, good))
	padded := padIn + "." + pad(macSig(sha256.New, secret, padIn))

	primary := viewerWith(later, func(c map[string]any) {
		c["rules"] = []any{map[string]any{
			"kind": "grace", "scope": "manifest", "duration_sec": 300,
			"token": handToken(t, hdrHS256, graceClaims(harnessHash), secret),
		}}
	})

	return []parseCase{
		// Accepted.
		{name: "valid HS256", token: goodTok, payload: good, get: true, stats: true},
		{name: "valid HS512", token: hs512, payload: good, get: true, stats: true},
		{name: "exp with a fraction", token: handToken(t, hdrHS256, viewerClaims(float64(later)+0.5), secret),
			payload: viewerClaims(float64(later) + 0.5), get: true, stats: true},
		{name: "iat and nbf in the past", token: handToken(t, hdrHS256, viewerWith(later, func(c map[string]any) {
			c["iat"], c["nbf"] = now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix()
		}), secret), payload: viewerWith(later, func(c map[string]any) {
			c["iat"], c["nbf"] = now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix()
		}), get: true, stats: true},
		// thp checks no audience, with either library.
		{name: "aud as an array", token: handToken(t, hdrHS256, viewerWith(later, func(c map[string]any) { c["aud"] = []string{"x"} }), secret),
			payload: viewerWith(later, func(c map[string]any) { c["aud"] = []string{"x"} }), get: true, stats: true},
		{name: "primary with a grace rule", token: handToken(t, hdrHS256, primary, secret), payload: primary, get: true, stats: true},
		// No exp: good for content, not for the stream.
		{name: "grace token (no exp)", token: handToken(t, hdrHS256, graceClaims(harnessHash), secret),
			payload: graceClaims(harnessHash), get: true},

		// Refused.
		{name: "wrong secret", token: handToken(t, hdrHS256, good, secret+"x")},
		{name: "wrong api key", token: goodTok, apiKey: "x"},
		{name: "expired a minute ago", token: handToken(t, hdrHS256, viewerClaims(now.Add(-time.Minute).Unix()), secret), quiet: true},
		// The two below were taken by the old library (dgrijalva/jwt-go).
		{name: "exp this second", token: handToken(t, hdrHS256, viewerClaims(now.Unix()), secret), quiet: true},
		{name: "exp not a number", token: handToken(t, hdrHS256, viewerClaims("never"), secret), quiet: true},
		{name: "iat ahead of thp's clock", token: handToken(t, hdrHS256, viewerWith(later, func(c map[string]any) { c["iat"] = now.Add(time.Minute).Unix() }), secret)},
		{name: "nbf ahead of thp's clock", token: handToken(t, hdrHS256, viewerWith(later, func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() }), secret)},
		{name: "alg none, no signature", token: noneIn + "."},
		{name: "alg none, HMAC signature", token: noneIn + "." + macSig(sha256.New, secret, noneIn)},
		{name: "RS256 under the caller's key", token: rsIn + "." + seg64(rsSig)},
		// Key confusion: the header names RS256, the signature is an HMAC
		// under thp's secret, as if the secret were the public key.
		{name: "RS256 header, HMAC under the secret", token: handToken(t, hdrRS256, good, secret)},
		{name: "EdDSA header", token: handToken(t, map[string]any{"alg": "EdDSA"}, good, secret)},
		{name: "no alg", token: handToken(t, map[string]any{"typ": "JWT"}, good, secret)},
		{name: "one segment", token: "abc", quiet: true},
		{name: "two segments", token: parts[0] + "." + parts[1], quiet: true},
		{name: "four segments", token: goodTok + ".x", quiet: true},
		{name: "not base64", token: "!!!.@@@.###"},
		{name: "header not JSON", token: seg64([]byte("nope")) + "." + parts[1] + "." + parts[2]},
		{name: "claims not an object", token: handToken(t, hdrHS256, []int{1, 2}, secret)},
		{name: "bearer prefix", token: "Bearer " + goodTok},
		{name: "padded segments", token: padded},
		{name: "no token", token: ""},
	}
}

// jsonRoundTrip is what a JSON decoder makes of v: numbers as float64,
// arrays as []any.
func jsonRoundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (c parseCase) key() string {
	if c.apiKey != "" {
		return c.apiKey
	}
	return "k"
}

// Claims.Get is the one place thp parses a token; every handler goes
// through it.
func TestClaimsGetTokens(t *testing.T) {
	const secret = "parse-test-secret"
	cl := &Claims{apiKey: "k", apiSecret: secret}
	for _, c := range parseCases(t, secret) {
		t.Run(c.name, func(t *testing.T) {
			claims, err := cl.Get(c.token, c.key())
			if !c.get {
				if err == nil {
					t.Fatalf("accepted with claims %v", claims)
				}
				if claims != nil {
					t.Errorf("refused, yet claims %v came back", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if want := jsonRoundTrip(t, c.payload); !reflect.DeepEqual(map[string]any(claims), want) {
				t.Errorf("claims %v, want %v", claims, want)
			}
		})
	}
}

// The claims downstream reads keep their types: exp a float64, as
// tokenExpiry and the old library read it, the rest strings.
func TestClaimsGetClaimTypes(t *testing.T) {
	const secret = "parse-test-secret"
	exp := time.Now().Add(time.Hour).Unix()
	claims, err := (&Claims{apiKey: "k", apiSecret: secret}).Get(handToken(t, hdrHS256, viewerClaims(exp), secret), "k")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := claims["exp"].(float64); !ok || int64(got) != exp {
		t.Errorf("exp = %#v, want float64 %d", claims["exp"], exp)
	}
	for k, want := range map[string]string{"role": "paid", "rate": "20M", "sessionID": "s-parse", "domain": "example.org", "hash": harnessHash} {
		if got, ok := claims[k].(string); !ok || got != want {
			t.Errorf("%s = %#v, want %q", k, claims[k], want)
		}
	}
	if got := tokenDomain(claims); got != "example.org" {
		t.Errorf("tokenDomain = %q", got)
	}
	if e, ok := tokenExpiry(claims); !ok || e.Unix() != exp {
		t.Errorf("tokenExpiry = %v, %v; want %d", e, ok, exp)
	}
}

// Without keys, Claims.Get lets everything through: a proxy with no auth
// configured. The migration must not change that either.
func TestClaimsGetWithoutKeys(t *testing.T) {
	claims, err := (&Claims{}).Get("", "")
	if err != nil || claims == nil || len(claims) != 0 {
		t.Errorf("got %v, %v; want empty claims and no error", claims, err)
	}
}

// proxyGet runs a token through proxyHTTP for path and returns the status
// and the level of its "failed to get claims" line, if any.
func (h *throttleHarness) proxyGet(t *testing.T, path, token, apiKey string) (int, logrus.Level, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path+"?token="+url.QueryEscape(token)+"&api-key="+url.QueryEscape(apiKey), nil)
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	logger, hook := logtest.NewNullLogger()
	logger.SetLevel(logrus.DebugLevel)
	rec := httptest.NewRecorder()
	h.web.proxyHTTP(rec, r, src, logrus.NewEntry(logger))
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "failed to get claims") {
			return rec.Code, e.Level, true
		}
	}
	return rec.Code, 0, false
}

// Every parse site takes what it took and refuses what it refused: content
// ("/"), the session-stats stream and the speed test.
func TestTokenParseSites(t *testing.T) {
	const secret = statsSecret
	cases := parseCases(t, secret)

	t.Run("content", func(t *testing.T) {
		h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
		h.web.claims = &Claims{apiKey: "k", apiSecret: secret}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				status, level, logged := h.proxyGet(t, "/"+harnessHash+"/Sintel/Sintel.mkv", c.token, c.key())
				if c.get {
					if status != http.StatusOK {
						t.Errorf("status %d, want 200", status)
					}
					return
				}
				if status != http.StatusForbidden {
					t.Fatalf("status %d, want 403", status)
				}
				if !logged {
					t.Fatal("no failed-to-get-claims line")
				}
				want := logrus.WarnLevel
				if c.quiet {
					want = logrus.DebugLevel
				}
				if level != want {
					t.Errorf("logged at %v, want %v", level, want)
				}
			})
		}
	})

	t.Run("session-stats", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				// A server each: the accepted cases share one stream key,
				// and a stream's slot frees only once the server sees the
				// client go.
				_, _, srv := newStatsWeb(t)
				s := openStats(t, srv.URL+"/session-stats/"+harnessHash+"?token="+url.QueryEscape(c.token)+"&api-key="+url.QueryEscape(c.key()))
				s.cancel()
				want := http.StatusForbidden
				if c.stats {
					want = http.StatusOK
				}
				if s.resp.StatusCode != want {
					t.Errorf("status %d, want %d", s.resp.StatusCode, want)
				}
			})
		}
	})

	t.Run("speedtest", func(t *testing.T) {
		_, _, srv := newStatsWeb(t)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				resp, err := http.Get(srv.URL + "/speedtest?size=1&token=" + url.QueryEscape(c.token) + "&api-key=" + url.QueryEscape(c.key()))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				want := http.StatusForbidden
				if c.get {
					want = http.StatusOK
				}
				if resp.StatusCode != want {
					t.Errorf("status %d, want %d", resp.StatusCode, want)
				}
			})
		}
	})
}

// The grace path end to end on parsed tokens: the primary's rule comes out
// of Claims.Get with the inner token intact, the manifest swaps it in, and
// the inner token opens its own torrent and no other.
func TestGraceTokenThroughParse(t *testing.T) {
	const secret = statsSecret
	cl := &Claims{apiKey: "k", apiSecret: secret}
	inner := handToken(t, hdrHS256, graceClaims(harnessHash), secret)
	primaryTok := handToken(t, hdrHS256, viewerWith(time.Now().Add(time.Hour).Unix(), func(c map[string]any) {
		c["rules"] = []any{map[string]any{"kind": "grace", "scope": "manifest", "duration_sec": 12, "token": inner}}
	}), secret)

	primary, err := cl.Get(primaryTok, "k")
	if err != nil {
		t.Fatal(err)
	}
	rule := findGraceRule(primary)
	if rule == nil || rule.Token != inner || rule.DurationSec != 12 {
		t.Fatalf("grace rule %+v, want the inner token for 12 s", rule)
	}
	body := []byte("#EXTM3U\n#EXTINF:6.0,\nv0-0.ts?token=" + primaryTok + "\n#EXTINF:6.0,\nv0-1.ts?token=" + primaryTok +
		"\n#EXTINF:6.0,\nv0-2.ts?token=" + primaryTok + "\n")
	out := string(RewriteManifest(body, primary, primaryTok))
	if !strings.Contains(out, "v0-0.ts?token="+inner+"\n") || !strings.Contains(out, "v0-1.ts?token="+inner+"\n") ||
		!strings.Contains(out, "v0-2.ts?token="+primaryTok+"\n") {
		t.Fatalf("manifest not swapped as expected:\n%s", out)
	}

	got, err := cl.Get(inner, "k")
	if err != nil {
		t.Fatalf("inner token refused: %v", err)
	}
	if want := jsonRoundTrip(t, graceClaims(harnessHash)); !reflect.DeepEqual(map[string]any(got), want) {
		t.Errorf("inner claims %v, want %v", got, want)
	}

	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1000))
	h.web.claims = cl
	if status, _, _ := h.proxyGet(t, "/"+harnessHash+"/Sintel/Sintel.mkv", inner, "k"); status != http.StatusOK {
		t.Errorf("inner token on its torrent: %d, want 200", status)
	}
	if status, _, _ := h.proxyGet(t, "/"+strings.Repeat("a", 40)+"/Sintel/Sintel.mkv", inner, "k"); status != http.StatusForbidden {
		t.Errorf("inner token on another torrent: %d, want 403", status)
	}
}

// thp's own exp requirement for the stream, without the library in front
// of it: the library refuses a non-number exp and exp's own second itself
// now, so the HTTP cases no longer reach this check.
func TestSessionStatsTokenSession(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	base := func(exp any) jwt.MapClaims {
		return jwt.MapClaims{"hash": harnessHash, "sessionID": "s1", "exp": exp}
	}
	cases := []struct {
		name   string
		claims jwt.MapClaims
		reason string
	}{
		{"valid", base(float64(now.Unix() + 1)), ""},
		{"no exp", jwt.MapClaims{"hash": harnessHash, "sessionID": "s1"}, "exp"},
		{"exp not a number", base("never"), "exp"},
		{"exp this second", base(float64(now.Unix())), "exp"},
		{"exp passed", base(float64(now.Unix() - 1)), "exp"},
		{"exp as json.Number", base(json.Number("1800000001")), ""},
		{"hash of another torrent", jwt.MapClaims{"hash": strings.Repeat("a", 40), "sessionID": "s1", "exp": float64(now.Unix() + 1)}, "hash"},
		{"no sessionID", jwt.MapClaims{"hash": harnessHash, "exp": float64(now.Unix() + 1)}, "sessionID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sid, _, reason := sessionStatsTokenSession(c.claims, harnessHash, now)
			if reason != c.reason {
				t.Errorf("reason %q, want %q", reason, c.reason)
			}
			if (sid == "") != (c.reason != "") {
				t.Errorf("sessionID %q with reason %q", sid, reason)
			}
		})
	}
}
