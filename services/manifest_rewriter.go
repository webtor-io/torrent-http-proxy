package services

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	tagSessionOffset = "#EXT-X-SESSION-OFFSET:"
	tagExtinf        = "#EXTINF:"
)

// maxGraceManifestBytes bounds the playlist rewriteManifestForGrace holds in
// memory, 4 times the largest through thp in an hour (2.0 MB, see
// HTTPProxy.get). Nothing else bounds it: a torrent's file named *.m3u8 comes
// from the seeder whole, on the primary token that carries the grace rule. A
// larger playlist is passed on as it came, its segments on the primary token.
const maxGraceManifestBytes = 8 << 20

// rewriteManifestForGrace is the response-rule handler registered in rules.go
// for `kind=grace, scope=manifest`. It intercepts .m3u8 responses and applies
// per-segment token swaps when the request's primary token carries a grace
// rule. No-op for non-m3u8 paths or when no grace rule is present.
func rewriteManifestForGrace(r *http.Response, rc *RulesContext) error {
	if r.Request == nil || !strings.HasSuffix(r.Request.URL.Path, ".m3u8") {
		return nil
	}
	if findGraceRule(rc.Claims) == nil {
		return nil
	}
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGraceManifestBytes+1))
	if err != nil {
		_ = r.Body.Close()
		return errors.Wrap(err, "failed to read manifest body")
	}
	if len(body) > maxGraceManifestBytes {
		logrus.WithFields(logrus.Fields{
			"path":  redactURL(r.Request.URL.Path),
			"limit": maxGraceManifestBytes,
		}).Warn("manifest too large for grace, passed on unrewritten")
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return nil
	}
	_ = r.Body.Close()
	rewritten := RewriteManifest(body, rc.Claims, rc.PrimaryToken)
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

// findGraceRule returns the first grace/manifest rule from a claims set,
// or nil if none. Empty/zero rules are skipped.
func findGraceRule(claims jwt.MapClaims) *Rule {
	for _, r := range ExtractRules(claims) {
		if r.Kind == "grace" && r.Scope == "manifest" && r.Token != "" && r.DurationSec > 0 {
			rr := r
			return &rr
		}
	}
	return nil
}

// parseSessionOffset extracts the value of #EXT-X-SESSION-OFFSET:<sec>.
// Returns 0 when the tag is missing or unparseable (treated as fresh start).
func parseSessionOffset(body []byte) float64 {
	idx := bytes.Index(body, []byte(tagSessionOffset))
	if idx < 0 {
		return 0
	}
	rest := body[idx+len(tagSessionOffset):]
	end := bytes.IndexAny(rest, "\r\n")
	if end < 0 {
		end = len(rest)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(rest[:end])), 64)
	if err != nil {
		return 0
	}
	return v
}

// stripSessionOffsetLine removes the #EXT-X-SESSION-OFFSET line from the
// manifest. Tag was injected upstream as a hint for THP only — players ignore
// unknown X- tags but downstream consumers (CDN logs, etc.) needn't see it.
func stripSessionOffsetLine(body []byte) []byte {
	out := make([]byte, 0, len(body))
	for line := range bytes.Lines(body) {
		if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte(tagSessionOffset)) {
			continue
		}
		out = append(out, line...)
	}
	return out
}

// RewriteManifest applies grace-rule token swaps to an HLS variant playlist.
//
// Behavior:
//   - No grace rule on the claims → returns body unchanged.
//   - sessionOffset already past graceUntil → returns body with offset tag
//     stripped (no segments qualify).
//   - Otherwise: walks #EXTINF/segment pairs, accumulates movie-time, and
//     replaces ?token=<primary> with ?token=<grace> on segment URL lines whose
//     movie-time start falls within [0, graceUntil). Segment-start semantics
//     match the design doc (movie_time(N) = offset + Σ EXTINF_0..N-1).
//
// Master playlists (#EXT-X-STREAM-INF) carry no #EXTINF and no segment URLs,
// so this function is a no-op on them and they pass through unchanged.
func RewriteManifest(body []byte, claims jwt.MapClaims, primaryToken string) []byte {
	rule := findGraceRule(claims)
	if rule == nil || primaryToken == "" {
		return body
	}

	sessionOffset := parseSessionOffset(body)
	graceUntil := float64(rule.DurationSec)
	if sessionOffset >= graceUntil {
		return stripSessionOffsetLine(body)
	}

	var out bytes.Buffer
	out.Grow(len(body))

	movieTime := sessionOffset
	pendingExtinf := 0.0 // duration of the next segment, set by EXTINF, consumed by following URL line

	// bytes.Lines, not SplitAfter: a slice header a line (24 B) would cost
	// a body of empty lines 24 times its size.
	for line := range bytes.Lines(body) {
		// Drop trailing newline for inspection but emit raw at end.
		trimmed := bytes.TrimRight(line, "\r\n")
		stripped := bytes.TrimLeft(trimmed, " \t")

		// Strip session-offset tag from output.
		if bytes.HasPrefix(stripped, []byte(tagSessionOffset)) {
			continue
		}

		// EXTINF: parse duration for the upcoming segment URL line.
		if bytes.HasPrefix(stripped, []byte(tagExtinf)) {
			rest := stripped[len(tagExtinf):]
			if comma := bytes.IndexByte(rest, ','); comma >= 0 {
				rest = rest[:comma]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(string(rest)), 64); err == nil {
				pendingExtinf = d
			}
			out.Write(line)
			continue
		}

		// Comments, blank lines, other tags: pass through.
		if len(stripped) == 0 || stripped[0] == '#' {
			out.Write(line)
			continue
		}

		// Segment URL line. Use start-of-segment movie time.
		if movieTime < graceUntil && pendingExtinf > 0 {
			line = swapToken(line, primaryToken, rule.Token)
		}
		movieTime += pendingExtinf
		pendingExtinf = 0
		out.Write(line)
	}

	return out.Bytes()
}

// swapToken replaces an exact ?token=<primary> or &token=<primary> occurrence
// in a segment URL line. JWT chars are URL-safe ([A-Za-z0-9_.-]) so substring
// match is sufficient. No-op if primary not present in the line.
func swapToken(line []byte, primary, grace string) []byte {
	if len(primary) == 0 {
		return line
	}
	needle := []byte("token=" + primary)
	if !bytes.Contains(line, needle) {
		return line
	}
	return bytes.Replace(line, needle, []byte("token="+grace), 1)
}
