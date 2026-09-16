package services

import (
	"context"
	"fmt"
	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	"github.com/webtor-io/torrent-http-proxy/services/k8s"
	"io"
	corev1 "k8s.io/api/core/v1"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli"
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

type ServiceLocation struct {
	*lazymap.LazyMap[*Location]
	ep           *k8s.Endpoints
	nodes        *k8s.NodesStat
	c            *cli.Context
	nn           string
	ignore       *EndpointIgnoreList
	probeChecker *ProbeChecker
}

type ProbeChecker struct {
	*lazymap.LazyMap[bool]
	cl *http.Client
}

func (s *ProbeChecker) Get(l *Location) (bool, error) {
	return s.LazyMap.Get(l.IP.String(), func() (bool, error) {
		probePort := l.Ports.Probe
		if probePort == 0 {
			probePort = l.Ports.HTTP
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%v:%v", l.IP, probePort), nil)
		if err != nil {
			return false, err
		}
		resp, err := s.cl.Do(req)
		if err != nil {
			return false, err
		}
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)
		if resp.StatusCode >= 500 {
			return false, errors.Errorf("unexpected status code: %d", resp.StatusCode)
		}
		return true, nil
	})
}

type EndpointIgnoreList struct {
	*lazymap.LazyMap[bool]
}

func (s *EndpointIgnoreList) Ignore(ip string) bool {
	res, _ := s.Get(ip, func() (bool, error) {
		return true, nil
	})
	return res
}

func (s *EndpointIgnoreList) IsIgnored(ip string) bool {
	_, ok := s.Status(ip)
	return ok
}

func NewServiceLocationPool(c *cli.Context, cl *http.Client, nodes *k8s.NodesStat, ep *k8s.Endpoints) *ServiceLocation {
	return &ServiceLocation{
		c:     c,
		ep:    ep,
		nodes: nodes,
		nn:    c.String(myNodeNameFlag),
		LazyMap: lazymap.New[*Location](&lazymap.Config{
			Expire: 15 * time.Second,
		}),
		ignore: &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{
			Expire: 30 * time.Second,
		})},
		probeChecker: &ProbeChecker{
			LazyMap: lazymap.New[bool](&lazymap.Config{
				Expire:      30 * time.Second,
				StoreErrors: true,
			}),
			cl: cl,
		},
	}
}

func (s *ServiceLocation) Get(cfg *ServiceConfig, src *Source, claims jwt.MapClaims) (*Location, error) {
	key := cfg.Name + src.InfoHash
	role, ok := claims["role"].(string)
	if ok {
		key += role
	}
	return s.LazyMap.Get(key, func() (*Location, error) {
		if cfg.EndpointsProvider == Kubernetes {
			return s.getKubernetesWithProbeCheck(cfg, src, claims)
		} else if cfg.EndpointsProvider == Environment {
			return s.getEnvironment(cfg)
		} else {
			return nil, errors.Errorf("unknown endpoints provider: %s", cfg.EndpointsProvider)
		}
	})
}

func (s *ServiceLocation) getKubernetesWithProbeCheck(cfg *ServiceConfig, src *Source, claims jwt.MapClaims) (*Location, error) {
	i := 0
	for {
		if i > 2 {
			log.Warnf("failed to get location for %v after %v tries", cfg.Name, i)
			return &Location{
				Unavailable: true,
			}, nil
		}
		l, err := s.getKubernetes(cfg, src, claims)
		if err != nil {
			return nil, err
		}
		if l.Unavailable {
			return l, nil
		}
		_, err = s.probeChecker.Get(l)
		if err != nil {
			log.WithError(err).Warnf("probe check failed for %v location %+v, add it to ignore", cfg.Name, l)
			s.ignore.Ignore(l.IP.String())
			i++
			continue
		}
		return l, nil
	}
}

func (s *ServiceLocation) getKubernetes(cfg *ServiceConfig, src *Source, claims jwt.MapClaims) (*Location, error) {
	endpoints, err := s.ep.Get(cfg.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get endpoints")
	}
	subset := endpoints.Subsets[0]
	as := subset.Addresses
	as = s.filterAddressesByIgnore(as)
	if len(as) == 0 {
		return &Location{
			Unavailable: true,
		}, nil
	}
	var a *corev1.EndpointAddress
	if !sha1R.Match([]byte(src.InfoHash)) {
		a = &as[rand.Intn(len(as))]
	} else if cfg.Distribution == Hash {
		a, err = s.distributeByHash(src, as)
	} else if cfg.Distribution == NodeHash {
		a, err = s.distributeByNodeHash(src, as, claims)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to distribute")
	}
	a, err = s.preferLocal(cfg, src, a, as)
	if err != nil {
		return nil, err
	}
	return s.addressToLocation(a, &subset), nil
}

// preferLocal swaps the distributed pick for a pod on this proxy's own node
// when the service asks for it (PreferLocalNode) — the viewer was sent to
// this node by rest-api, so the local pod is the one with the warm disk.
//
// Never for an internal caller. A service inside the cluster (subtitle
// translation following a transcoder session, say) reaches the proxy's
// ClusterIP on whichever node its own pod runs; "local" there is a random
// node, and a transcoder session lives on exactly one pod — the rendezvous
// one the viewer's own requests reached. Overriding sent the 2026-09-16
// live-subtitle poll to a pod that had never seen the session: 404.
func (s *ServiceLocation) preferLocal(cfg *ServiceConfig, src *Source, a *corev1.EndpointAddress, as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	if a == nil || s.nn == "" || *a.NodeName == s.nn || !cfg.PreferLocalNode || src.Internal {
		return a, nil
	}
	var las []corev1.EndpointAddress
	for _, c := range as {
		if *c.NodeName == s.nn {
			las = append(las, c)
		}
	}
	if len(las) == 0 {
		return a, nil
	}
	la, err := s.distributeByHash(src, las)
	if err != nil {
		return nil, errors.Wrap(err, "failed to distribute locally")
	}
	return la, nil
}

func (s *ServiceLocation) getPort(sub *corev1.EndpointSubset, name string) int {
	for _, p := range sub.Ports {
		if p.Name == name {
			return int(p.Port)
		}
	}
	return 0
}

func (s *ServiceLocation) addressToLocation(a *corev1.EndpointAddress, sub *corev1.EndpointSubset) *Location {
	if a == nil {
		return &Location{
			Unavailable: true,
		}
	}
	return &Location{
		IP: net.ParseIP(a.IP),
		Ports: Ports{
			HTTP:  s.getPort(sub, "http"),
			Probe: s.getPort(sub, "httpprobe"),
		},
		Unavailable: false,
	}
}

// pickByRendezvous chooses the pod that owns the infohash in rendezvous
// order over pod IPs (see rendezvous.go). The interval partition this
// replaced sorted pods by IP as a string, so a pod restarting on a new IP
// shifted the boundaries of every pod between its old and new position
// and their torrents migrated to cold pods (duplicated across two pods
// until the cleaner reaped the stale copy).
//
// GetFallback keeps its guarantee for free: excluding the failed IP leaves
// the runner-up for that infohash, the same one on every proxy instance.
func pickByRendezvous(infoHash string, as []corev1.EndpointAddress) *corev1.EndpointAddress {
	ips := make([]string, 0, len(as))
	for i := range as {
		ips = append(ips, as[i].IP)
	}
	owner := rendezvousPick(infoHash, ips)
	for i := range as {
		if as[i].IP == owner {
			return &as[i]
		}
	}
	return nil
}

func (s *ServiceLocation) distributeByHash(src *Source, as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	if len(as) == 0 {
		return nil, nil
	}
	return pickByRendezvous(src.InfoHash, as), nil
}

func (s *ServiceLocation) distributeByNodeHash(src *Source, as []corev1.EndpointAddress, claims jwt.MapClaims) (*corev1.EndpointAddress, error) {
	sort.Slice(as, func(i, j int) bool {
		return as[i].IP < as[j].IP
	})
	nodesM := map[string]bool{}
	var nodes []string
	for _, a := range as {
		nodesM[*a.NodeName] = true
	}
	for n := range nodesM {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	nodes, err := s.filterNodesByRole(nodes, claims)
	if err != nil {
		return nil, errors.Wrap(err, "failed to filter nodes by role")
	}
	if len(nodes) == 0 {
		return nil, errors.Errorf("failed to distribute, no nodes found")
	}
	// Node step: rendezvous over node names — rest-api ranks the same way
	// (services/subdomains.go) to send the client to the node this proxy
	// calls home, and lists the runners-up as its fallbacks. Until
	// 2026-09-07 this was an interval partition over nodes sorted by name:
	// a node joining or leaving shifted every boundary after it and about
	// half the hashes changed node (cold per-node disk caches).
	node := rendezvousPick(src.InfoHash, nodes)
	var nas []corev1.EndpointAddress
	for _, a := range as {
		if *a.NodeName == node {
			nas = append(nas, a)
		}
	}
	return pickByRendezvous(src.InfoHash, nas), nil
}

func (s *ServiceLocation) filterNodesByRole(nodes []string, claims jwt.MapClaims) ([]string, error) {
	if claims == nil {
		return nodes, nil
	}
	role, ok := claims["role"].(string)
	if !ok {
		return nodes, nil
	}
	if role == "" {
		return nodes, nil
	}
	ns, err := s.nodes.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get nodes")
	}
	var res []string
	for _, n := range nodes {
		for _, nss := range ns {
			if n == nss.Name && nss.IsAllowed(role) {
				res = append(res, n)
			}
		}
	}
	return res, nil
}

func (s *ServiceLocation) getEnvironment(cfg *ServiceConfig) (*Location, error) {
	name := strings.ReplaceAll(strings.ToUpper(cfg.Name), "-", "_")
	portName := name + "_SERVICE_PORT"
	hostName := name + "_SERVICE_HOST"
	port, err := strconv.Atoi(os.Getenv(portName))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse environment variable %s with value \"%v\"", portName, os.Getenv(portName))
	}
	ip := net.ParseIP(os.Getenv(hostName))
	if ip == nil {
		return nil, errors.Errorf("failed to parse environment variable %v with value \"%v\"", hostName, os.Getenv(hostName))
	}
	return &Location{
		Ports: Ports{
			HTTP: port,
		},
		IP: ip,
	}, nil
}

// GetFallback resolves a fallback location for retry.
// For Kubernetes: adds excludeIP to ignore list and re-runs the same resolution
// logic as getKubernetes. NodeHash distribution guarantees the same infohash
// lands on the same node, so no extra node validation is needed.
// For Environment: returns the same static location (retry to same host).
func (s *ServiceLocation) GetFallback(cfg *ServiceConfig, src *Source, excludeIP net.IP, claims jwt.MapClaims) (*Location, error) {
	if cfg.EndpointsProvider == Environment {
		return s.getEnvironment(cfg)
	}

	// Temporarily ignore the failed IP.
	s.ignore.Ignore(excludeIP.String())

	// Run the same resolution logic (without cache).
	loc, err := s.getKubernetes(cfg, src, claims)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve fallback")
	}
	if loc.Unavailable {
		return nil, errors.Errorf("no available pods after excluding %s", excludeIP)
	}

	return loc, nil
}

func (s *ServiceLocation) filterAddressesByIgnore(as []corev1.EndpointAddress) []corev1.EndpointAddress {
	var res []corev1.EndpointAddress
	for _, a := range as {
		if s.ignore.IsIgnored(net.ParseIP(a.IP).String()) {
			continue
		}
		res = append(res, a)
	}
	return res
}
