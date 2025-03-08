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
	lazymap.LazyMap[*Location]
	ep     *k8s.Endpoints
	nodes  *k8s.NodesStat
	c      *cli.Context
	cl     *http.Client
	nn     string
	ignore *EndpointIgnoreList
}

type EndpointIgnoreList struct {
	lazymap.LazyMap[bool]
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
		cl:    cl,
		nodes: nodes,
		nn:    c.String(myNodeNameFlag),
		LazyMap: lazymap.New[*Location](&lazymap.Config{
			Expire: 15 * time.Second,
		}),
		ignore: &EndpointIgnoreList{lazymap.New[bool](&lazymap.Config{
			Expire: 30 * time.Second,
		})},
	}
}

func (s *ServiceLocation) Get(cfg *ServiceConfig, src *Source, claims jwt.MapClaims) (*Location, error) {
	key := cfg.Name + src.InfoHash
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

func (s *ServiceLocation) checkProbe(l *Location) error {
	probePort := l.Ports.Probe
	if probePort == 0 {
		probePort = l.Ports.HTTP
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%v:%v", l.IP, probePort), nil)
	if err != nil {
		return err
	}
	resp, err := s.cl.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	if resp.StatusCode < 500 {
		return nil
	}
	return errors.Errorf("unexpected status code: %d", resp.StatusCode)
}

func (s *ServiceLocation) getKubernetesWithProbeCheck(cfg *ServiceConfig, src *Source, claims jwt.MapClaims) (*Location, error) {
	l, err := s.getKubernetes(cfg, src, claims)
	if err != nil {
		return nil, err
	}
	if l.Unavailable {
		return l, nil
	}
	err = s.checkProbe(l)
	if err != nil {
		log.WithError(err).Warnf("probe check failed for location %+v, add it to ignore", l)
		s.ignore.Ignore(l.IP.String())
		//s.ep.Drop(cfg.Name)
		//s.nodes.Drop("")
	}
	return s.getKubernetes(cfg, src, claims)
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
	if a != nil && s.nn != "" && *a.NodeName != s.nn && cfg.PreferLocalNode {
		var las []corev1.EndpointAddress
		for _, a := range as {
			if *a.NodeName == s.nn {
				las = append(las, a)
			}
		}
		if len(las) > 0 {
			a, err = s.distributeByHash(src, las)
			if err != nil {
				return nil, errors.Wrap(err, "failed to distribute locally")
			}
		}
	}
	return s.addressToLocation(a, &subset), nil
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

func (s *ServiceLocation) distributeByHash(src *Source, as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	sort.Slice(as, func(i, j int) bool {
		return as[i].IP < as[j].IP
	})
	hex := src.InfoHash[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse hex from infohash=%v", src.InfoHash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	interval := total / len(as)
	for i := 0; i < len(as); i++ {
		if num < (i+1)*interval {
			return &as[i], nil
		}
	}
	return nil, nil
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
	hex := src.InfoHash[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse hex from infohash=%v", src.InfoHash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	nodeInterval := total / len(nodes)
	for i := 0; i < len(nodes); i++ {
		var nas []corev1.EndpointAddress
		for _, a := range as {
			if *a.NodeName == nodes[i] {
				nas = append(nas, a)
			}
		}
		aInterval := nodeInterval / len(nas)
		for j := 0; j < len(nas); j++ {
			if num < i*nodeInterval+(j+1)*aInterval {
				return &nas[j], nil
			}
		}
	}
	return nil, nil
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
