package services

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"

	"github.com/webtor-io/torrent-http-proxy/services/k8s"
)

const (
	internalCallerAddrsFlag = "internal-caller-addrs"

	// internalCallersInterval is how often the set is rebuilt from the
	// endpoints cache routing reads (k8s.Endpoints, 60 s): a pod routing has
	// just started sending to is a known caller within about a second.
	// Reading the cache costs nothing; the API is asked once a minute per
	// service, when an entry expires.
	internalCallersInterval = time.Second

	// internalCallerLinger is how long an address stays internal after the
	// last listing that had it. A pod leaves its Endpoints as it starts
	// terminating and keeps serving, and fetching through thp, for its grace
	// period: 90 s for the archiver, 45 s for the transcoder (charts,
	// 2026-09-27). It also carries the set over an API error, but not
	// beyond it: then a caller reads external, never the reverse.
	internalCallerLinger = 2 * time.Minute

	// internalCallersRetry is how long a service whose endpoints failed is
	// left alone. The endpoints cache keeps no errors: without it a service
	// missing from the cluster would be asked for, and warned about, every
	// interval.
	internalCallersRetry = 15 * time.Second

	// configuredCaller is the caller name of an --internal-caller-addrs address.
	configuredCaller = "configured"
)

var promInternalCallers = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "webtor_http_proxy_internal_callers",
	Help: "HTTP Proxy addresses whose requests are internal (pods of the services it routes to, plus configured ones)",
})

func init() {
	prometheus.MustRegister(promInternalCallers)
}

func RegisterInternalCallersFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   internalCallerAddrsFlag,
			Usage:  "comma-separated IP addresses whose requests are internal (no limits, no session accounting) besides the pods of the services located through Kubernetes endpoints: for services located through the environment. Exact addresses, no ranges. A front proxy must not connect from any of them",
			EnvVar: "INTERNAL_CALLER_ADDRS",
		},
	)
}

// InternalCallers answers who is calling: whether a connection's TCP peer is
// one of our own services fetching on a viewer's behalf, with the viewer's
// token, while serving the viewer's own request. nginx-vod reads the mp4 its
// segments are cut from (and passes the viewer's X-Forwarded-For on),
// content-transcoder's FFmpeg and content-prober's ffprobe read the source,
// the archiver reads each file, srt2vtt, video-info and subtitle-translate
// read theirs. The viewer's own request to that service came through thp and
// already took its limiter slot and its bytes out of the session's bucket:
// the service's fetch taking them again limits the viewer twice.
//
// Decided by the transport alone: the connection's peer address (RemoteAddr
// as net/http sets it from the accepted connection) against the pods behind
// the Kubernetes endpoints of every service in the services config, the same
// endpoints routing picks from, plus --internal-caller-addrs. Never a header:
// X-Forwarded-For and the like are the client's to write, and nginx-vod
// forwards the viewer's. Everything else is external: ingress-nginx, a pod
// of a service thp does not route to, an address not listed yet.
//
// Routing is a separate question with its own answer (Source.Internal, from
// X-Forwarded-For): an internal caller is not re-routed by this.
type InternalCallers struct {
	names    []string
	get      func(name string) (*corev1.Endpoints, error)
	static   map[netip.Addr]string
	interval time.Duration
	linger   time.Duration
	retry    time.Duration
	now      func() time.Time

	// addrs is the current snapshot, address → service name; read on every
	// request, replaced whole by refresh.
	addrs atomic.Pointer[map[netip.Addr]string]

	// mu serializes refresh; seen and retryAt are its state: when each
	// address was last listed, and by which service; when a service whose
	// endpoints failed is asked for again.
	mu      sync.Mutex
	seen    map[netip.Addr]seenCaller
	retryAt map[string]time.Time

	closing   chan struct{}
	closeOnce sync.Once
	startOnce sync.Once
}

type seenCaller struct {
	name string
	at   time.Time
}

// NewInternalCallers takes its services from cfg: every entry located
// through Kubernetes endpoints. An entry located through the environment
// contributes nothing: its address is where to send a request (a Service's
// virtual IP in a cluster, a loopback shared by every process in a single
// container), not whose connection it is, so its callers are listed with
// --internal-caller-addrs or are external.
func NewInternalCallers(c *cli.Context, cfg *ServicesConfig, ep *k8s.Endpoints) (*InternalCallers, error) {
	static, err := parseCallerAddrs(c.String(internalCallerAddrsFlag))
	if err != nil {
		return nil, err
	}
	names := internalCallerNames(cfg)
	ic := newInternalCallers(names, ep.Get, static, internalCallersInterval, internalCallerLinger, time.Now)
	if len(names) == 0 && len(static) == 0 {
		// The absence explains itself here rather than as limits nobody
		// expected on a transcoder's source reads.
		log.Warn("no internal callers: no service is located through Kubernetes endpoints and --internal-caller-addrs is empty, so every request is external, a service's fetch on a viewer's behalf included (limiters, session stats)")
	} else {
		log.WithFields(log.Fields{
			"services":   strings.Join(names, ","),
			"configured": len(static),
		}).Info("internal callers: pods of the routed services and configured addresses")
	}
	return ic, nil
}

func newInternalCallers(names []string, get func(string) (*corev1.Endpoints, error), static []netip.Addr, interval, linger time.Duration, now func() time.Time) *InternalCallers {
	ic := &InternalCallers{
		names:    names,
		get:      get,
		static:   make(map[netip.Addr]string, len(static)),
		interval: interval,
		linger:   linger,
		retry:    internalCallersRetry,
		now:      now,
		seen:     map[netip.Addr]seenCaller{},
		retryAt:  map[string]time.Time{},
		closing:  make(chan struct{}),
	}
	for _, a := range static {
		ic.static[a] = configuredCaller
	}
	// Configured addresses are internal from the first request on; the
	// endpoints' pods from the first refresh.
	ic.publish(map[netip.Addr]seenCaller{})
	return ic
}

// internalCallerNames lists the Kubernetes-located services of cfg, each once.
func internalCallerNames(cfg *ServicesConfig) []string {
	if cfg == nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, sc := range *cfg {
		if sc != nil && sc.EndpointsProvider == Kubernetes && sc.Name != "" {
			set[sc.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func parseCallerAddrs(raw string) ([]netip.Addr, error) {
	var res []netip.Addr
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid --%s entry %q: an IP address is expected", internalCallerAddrsFlag, s)
		}
		res = append(res, a.Unmap())
	}
	return res, nil
}

// endpointAddrs is every pod address of ep: all subsets, ready or not. A pod
// failing its readiness probe still finishes what it started, through thp.
func endpointAddrs(ep *corev1.Endpoints) []netip.Addr {
	if ep == nil {
		return nil
	}
	var res []netip.Addr
	for _, s := range ep.Subsets {
		for _, as := range [][]corev1.EndpointAddress{s.Addresses, s.NotReadyAddresses} {
			for _, a := range as {
				if ip, err := netip.ParseAddr(a.IP); err == nil {
					res = append(res, ip.Unmap())
				}
			}
		}
	}
	return res
}

// Caller returns the name of the service whose pod remoteAddr (an
// http.Request's RemoteAddr: "ip:port") is, and whether it is one. IPv4
// peers a dual-stack listener reports as IPv4-mapped IPv6 are compared as
// IPv4, like every listed address. A nil InternalCallers knows none: every
// caller is external.
func (c *InternalCallers) Caller(remoteAddr string) (string, bool) {
	if c == nil {
		return "", false
	}
	m := c.addrs.Load()
	if m == nil {
		return "", false
	}
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", false
	}
	name, ok := (*m)[ap.Addr().Unmap()]
	return name, ok
}

// refresh reads every service's endpoints (from the cache routing shares)
// and publishes the addresses listed now or within the linger. A service
// whose endpoints fail keeps its addresses only as long as the linger.
func (c *InternalCallers) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, name := range c.names {
		if now.Before(c.retryAt[name]) {
			continue
		}
		ep, err := c.get(name)
		if err != nil {
			c.retryAt[name] = now.Add(c.retry)
			log.WithError(err).WithField("service", name).Warn("failed to get endpoints of internal callers")
			continue
		}
		for _, a := range endpointAddrs(ep) {
			c.seen[a] = seenCaller{name: name, at: now}
		}
	}
	for a, s := range c.seen {
		if now.Sub(s.at) > c.linger {
			delete(c.seen, a)
		}
	}
	c.publish(c.seen)
}

func (c *InternalCallers) publish(seen map[netip.Addr]seenCaller) {
	m := make(map[netip.Addr]string, len(seen)+len(c.static))
	for a, s := range seen {
		m[a] = s.name
	}
	for a, n := range c.static {
		m[a] = n
	}
	c.addrs.Store(&m)
	promInternalCallers.Set(float64(len(m)))
}

// Start refreshes at once, in the background, and then every interval until
// Close. Requests before the first refresh completes read external. Nothing
// to refresh without Kubernetes-located services.
func (c *InternalCallers) Start() {
	if c == nil || len(c.names) == 0 {
		return
	}
	c.startOnce.Do(func() {
		go func() {
			t := time.NewTicker(c.interval)
			defer t.Stop()
			for {
				c.refresh()
				select {
				case <-c.closing:
					return
				case <-t.C:
				}
			}
		}()
	})
}

// Close stops the refresh. It does not wait for one in progress, which may
// sit in an API call up to its timeout; the snapshot stays readable.
func (c *InternalCallers) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() { close(c.closing) })
}
