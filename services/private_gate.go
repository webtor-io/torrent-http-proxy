package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	"github.com/webtor-io/torrent-http-proxy/services/uploads"
)

const (
	privateLookupTimeout  = 3 * time.Second
	privateLookupErrorTTL = 30 * time.Second
)

const (
	restAPIHostFlag = "rest-api-host"
	restAPIPortFlag = "rest-api-port"
)

func RegisterPrivateGateFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   restAPIHostFlag,
			Usage:  "rest-api host the private torrent gate asks whether a torrent is private; empty turns the gate off",
			EnvVar: "REST_API_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   restAPIPortFlag,
			Usage:  "rest-api port",
			Value:  80,
			EnvVar: "REST_API_SERVICE_PORT",
		},
	)
}

// NewPrivateGate is the gate on rest-api at REST_API_SERVICE_HOST, nil without
// it. An API secret without it is an error, run() returns it before anything
// starts: the tokens would be checked, and a private torrent served on any.
func NewPrivateGate(c *cli.Context) (*PrivateGate, error) {
	host := c.String(restAPIHostFlag)
	if host == "" {
		if c.String(apiSecretFlag) != "" {
			return nil, errors.New("private torrents: API_SECRET is set but REST_API_SERVICE_HOST is not; the private torrent gate asks rest-api whether a torrent is private: set REST_API_SERVICE_HOST and REST_API_SERVICE_PORT")
		}
		log.Info("private torrents gate: off, REST_API_SERVICE_HOST is not set")
		return nil, nil
	}
	g := newPrivateGate("http://"+net.JoinHostPort(host, strconv.Itoa(c.Int(restAPIPortFlag))), privateLookupTimeout, privateLookupErrorTTL)
	for _, o := range gateOutcomes {
		promPrivateGate.WithLabelValues(o)
	}
	log.Info("private torrents gate: on")
	return g, nil
}

// The outcomes of PrivateGate.check, the values of the outcome label.
const (
	gatePublic        = "public"
	gateAllowed       = "allowed"
	gateUnbound       = "unbound"
	gateNoUid         = "no_uid"
	gateLookupError   = "lookup_error"
	gateMalformedHash = "malformed_hash"
)

// gateOutcomes lists them all: their series are created at start.
var gateOutcomes = []string{gatePublic, gateAllowed, gateUnbound, gateNoUid, gateLookupError, gateMalformedHash}

var promPrivateGate = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "webtor_http_proxy_private_gate_total",
	Help: "HTTP Proxy requests for a torrent the private torrent gate checked, by outcome",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(promPrivateGate)
}

// PrivateGate lets a request for a private torrent through only with a token
// bound to its hash. Whether a hash is private it asks rest-api, without a
// token, and keeps the answer for good: a torrent does not change privacy. An
// error is kept errTTL, so an outage asks rest-api once per hash per errTTL,
// not on every viewer request, and a hash is not refused until the pod
// restarts. An upload ID is decided by the ID, without rest-api: kind 0 is
// private, the other kinds public.
type PrivateGate struct {
	url     string
	client  *http.Client
	privacy *lazymap.LazyMap[bool]
}

func newPrivateGate(restAPIURL string, timeout, errTTL time.Duration) *PrivateGate {
	return &PrivateGate{
		url:    restAPIURL,
		client: &http.Client{Timeout: timeout},
		// Concurrency: lazymap's default is 10 slots over all keys; with
		// rest-api hanging up to the timeout, new hashes would queue for
		// the slots longer than that.
		privacy: lazymap.New[bool](&lazymap.Config{Expire: 0, ErrorExpire: errTTL, StoreErrors: true, Capacity: 100000, Concurrency: 64}),
	}
}

// lookupError is a lookup that told neither private nor public. status is what
// the gate refuses with: 403 for a ban, 404, else 503. cause is fixed text,
// never rest-api's body.
type lookupError struct {
	status int
	cause  string
}

func (e *lookupError) Error() string { return e.cause }

// gateVerdict is check's decision: outcome, cause (a failed lookup's) and the
// status to refuse with; 0 lets the request through.
type gateVerdict struct {
	outcome, cause string
	status         int
}

// check decides a request for src's torrent on claims and counts the outcome.
// In order: a hash that is not 40 hex is malformed_hash; a lookup that failed
// is lookup_error with its status; a public torrent passes any token. A
// private one needs a token bound to its hash (unbound) that carries a uid
// (no_uid): role=vault included, internal too, except on an upload of kind 0.
func (g *PrivateGate) check(claims jwt.MapClaims, src *Source) gateVerdict {
	v := g.decide(claims, strings.ToLower(src.InfoHash))
	promPrivateGate.WithLabelValues(v.outcome).Inc()
	return v
}

func (g *PrivateGate) decide(claims jwt.MapClaims, h string) gateVerdict {
	if !isInfoHash(h) {
		return gateVerdict{gateMalformedHash, "", http.StatusForbidden}
	}
	private, err := uploads.Owned(h), error(nil) // kind 0 private, kinds 1–f public; no rest-api, no cache
	if !uploads.HomeHosted(h) {
		private, err = g.isPrivate(h)
	}
	if err != nil {
		// Not only rest-api's answers: lazymap returns a panic in the
		// lookup as a PanicError. Refused as an outage, never public.
		le := &lookupError{http.StatusServiceUnavailable, "lookup failed"}
		errors.As(err, &le)
		return gateVerdict{gateLookupError, le.cause, le.status}
	}
	if !private {
		return gateVerdict{outcome: gatePublic}
	}
	if bound, _ := claims["hash"].(string); bound != h {
		return gateVerdict{gateUnbound, "", http.StatusForbidden}
	}
	// Server calls without a user carry internal instead: rest-api's manifest,
	// web-ui's delete and its seal of an upload before a pledge. Uploads only:
	// on a private torrent internal is still no_uid.
	if uid, _ := claims["uid"].(string); uid == "" && !(uploads.Owned(h) && claims["internal"] == true) {
		return gateVerdict{gateNoUid, "", http.StatusForbidden}
	}
	return gateVerdict{outcome: gateAllowed}
}

func (g *PrivateGate) isPrivate(h string) (bool, error) {
	return g.privacy.Get(h, func() (bool, error) { return g.lookup(h) })
}

// lookup asks rest-api once. 200 with the field gives its value; a 403 with
// private_torrent (refused without a bound token) or private_unavailable (the
// store has no key or S3 for private records) is private. Anything else is an
// error, never "public": a 403 without those codes is a ban or the stoplist, a
// 200 without the field is an old rest-api.
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
