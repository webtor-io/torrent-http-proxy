package services

import "regexp"

// Credentials ride in every URL thp serves: a viewer's token (a JWT whose
// claims carry the session, its IP and User-Agent) and the API client's key,
// in the query -- and percent-encoded, once or twice, where a URL is passed
// on inside another (`token%3D...`). The request's log fields kept them all:
// ~300k lines an hour with a key, ~5k with a token (2026-09-30).

// credentialParam is a credential parameter and its value: the name at the
// start of a query parameter, raw or encoded (`?`, `&`, `%3F`, `%26`, and
// their double encodings), then `=` in any of those encodings. The value is
// what a token or a key is made of (base64url, dots, a UUID); it ends where
// the next parameter begins, encoded or not.
var credentialParam = regexp.MustCompile(`(?i)((?:^|[?&;]|%3F|%26|%253F|%2526)(?:token|api-key|api_key|apikey)(?:=|%3D|%253D))(?:[A-Za-z0-9._~-]|%2[Ee])+`)

// jwtLike is a JWT wherever it stands -- a path segment too (a signed link).
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)

const redacted = "<redacted>"

// redactURL is s as the logs may keep it: the values of its credential
// parameters, and any JWT, replaced by "<redacted>"; everything else as is.
func redactURL(s string) string {
	s = credentialParam.ReplaceAllString(s, "${1}"+redacted)
	return jwtLike.ReplaceAllString(s, redacted)
}
