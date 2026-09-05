package services

import (
	"crypto/sha1"
	"encoding/binary"
	"sort"
	"strings"
)

// rendezvousOrder ranks ids for an infohash by sha1(infohash, id)
// descending — highest-random-weight hashing. The first id owns the
// infohash, the rest are its fallbacks in order. When an id leaves, only
// the infohashes it owned move (each to its own runner-up); when one
// joins, it takes an even share from every other id.
//
// rest-api carries a copy of this function (services/rendezvous.go) and
// ranks nodes with it to send a client to the node this proxy will call
// home; both are pinned to the same literal test vector. Change one,
// change the other, regenerate the vector.
func rendezvousOrder(infoHash string, ids []string) []string {
	key := strings.ToLower(infoHash)
	type ranked struct {
		id    string
		score uint64
	}
	rs := make([]ranked, 0, len(ids))
	for _, id := range ids {
		sum := sha1.Sum([]byte(key + "\x00" + id))
		rs = append(rs, ranked{id: id, score: binary.BigEndian.Uint64(sum[:8])})
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].score != rs[j].score {
			return rs[i].score > rs[j].score
		}
		return rs[i].id < rs[j].id
	})
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.id)
	}
	return out
}

// rendezvousPick is the owner of infoHash among ids, "" when ids is empty.
func rendezvousPick(infoHash string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return rendezvousOrder(infoHash, ids)[0]
}
