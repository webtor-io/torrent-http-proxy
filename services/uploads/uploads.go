// Package uploads tells the IDs of uploaded content from torrent infohashes.
// An ID is the prefix, a kind (one hex digit) and 27 hex, all lower case. The
// same two predicates live in every service that reads these IDs, with the
// same test vectors: no dependencies, so the copies stay word for word.
package uploads

import "strings"

const prefix = "9c5db5752b7c"

// HomeHosted answers: does the ID's content live on its home node rather than
// in a swarm? Any kind. Strict: 40 characters, lower-case hex only, the prefix.
func HomeHosted(id string) bool {
	if len(id) != 40 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Owned answers: does the ID have one owner, the only one who writes it and
// the only one who sees it? Kind 0, a user's upload.
func Owned(id string) bool {
	return HomeHosted(id) && id[12] == '0'
}
