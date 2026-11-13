package services

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/webtor-io/lazymap"
)

const (
	privateLookupTimeout  = 3 * time.Second
	privateLookupErrorTTL = 30 * time.Second
)

// PrivateGate lets a request for a private torrent through only with a token
// bound to its hash. Whether a hash is private it asks rest-api, without a
// token, and keeps the answer for good: a torrent does not change privacy. An
// error is kept errTTL, so an outage asks rest-api once per hash per errTTL,
// not on every viewer request, and a hash is not refused until the pod
// restarts.
type PrivateGate struct {
	url     string
	client  *http.Client
	enforce bool
	privacy *lazymap.LazyMap[bool]
}

func newPrivateGate(restAPIURL string, enforce bool, timeout, errTTL time.Duration) *PrivateGate {
	return &PrivateGate{
		url:     restAPIURL,
		client:  &http.Client{Timeout: timeout},
		enforce: enforce,
		// Concurrency: lazymap's default is 10 slots over all keys; with
		// rest-api hanging up to the timeout, new hashes would queue for
		// the slots longer than that.
		privacy: lazymap.New[bool](&lazymap.Config{Expire: 0, ErrorExpire: errTTL, StoreErrors: true, Capacity: 100000, Concurrency: 64}),
	}
}

// lookupError is a lookup that told neither private nor public. status is what
// the gate answers in enforce: 403 for a ban, 404, else 503. cause is fixed
// text, never rest-api's body.
type lookupError struct {
	status int
	cause  string
}

func (e *lookupError) Error() string { return e.cause }

func (g *PrivateGate) isPrivate(h string) (bool, error) {
	return g.privacy.Get(h, func() (bool, error) { return g.lookup(h) })
}

// lookup asks rest-api once. 200 with the field gives its value; a 403 with
// private_torrent (refused without a bound token) or private_unavailable (the
// store has no key or S3 for private records) is private. Anything else is an error, never
// "public": a 403 without those codes is a ban or the stoplist, a 200 without
// the field is an old rest-api.
func (g *PrivateGate) lookup(h string) (bool, error) {
	res, err := g.client.Get(g.url + "/resource/" + h)
	if err != nil {
		return false, &lookupError{http.StatusServiceUnavailable, "rest-api unreachable"}
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Private *bool  `json:"private"`
		Code    string `json:"code"`
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(b, &body)
	}
	switch s := res.StatusCode; {
	case s == http.StatusOK && err != nil:
		return false, &lookupError{http.StatusServiceUnavailable, "rest-api bad body"}
	case s == http.StatusOK && body.Private == nil:
		return false, &lookupError{http.StatusServiceUnavailable, "rest-api 200 without private"}
	case s == http.StatusOK:
		return *body.Private, nil
	case s == http.StatusForbidden && (body.Code == "private_torrent" || body.Code == "private_unavailable"):
		return true, nil
	case s == http.StatusForbidden, s == http.StatusNotFound:
		return false, &lookupError{s, fmt.Sprintf("rest-api %d", s)}
	default:
		return false, &lookupError{http.StatusServiceUnavailable, fmt.Sprintf("rest-api %d", s)}
	}
}
