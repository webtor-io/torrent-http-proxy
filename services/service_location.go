package services

import (
	"math/rand"
	"net"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	corev1 "k8s.io/api/core/v1"
)

type DISTRIBUTION int

const (
	HASH DISTRIBUTION = iota
	NODE_HASH
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

type ServiceLocation struct {
	loc    *Location
	cfg    *ServiceConfig
	ep     *EndpointsPool
	inited bool
	err    error
	mux    sync.Mutex
	nn     string
	params *InitParams
}

func NewServiceLocation(c *cli.Context, cfg *ServiceConfig, params *InitParams, ep *EndpointsPool) *ServiceLocation {
	return &ServiceLocation{
		cfg:    cfg,
		ep:     ep,
		nn:     c.String(MY_NODE_NAME),
		params: params,
	}
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
			GRPC:  s.getPort(sub, "grpc"),
			Probe: s.getPort(sub, "httpprobe"),
		},
		Unavailable: false,
	}
}

func (s *ServiceLocation) distributeByHash(as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	sort.Slice(as, func(i, j int) bool {
		return as[i].IP > as[j].IP
	})
	hex := s.params.InfoHash[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse hex from infohash=%v", s.params.InfoHash)
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

func (s *ServiceLocation) distributeByNodeHash(as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	sort.Slice(as, func(i, j int) bool {
		return as[i].IP > as[j].IP
	})
	nodesM := map[string]bool{}
	nodes := []string{}
	for _, a := range as {
		nodesM[*a.NodeName] = true
	}
	for n := range nodesM {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	hex := s.params.InfoHash[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse hex from infohash=%v", s.params.InfoHash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	nodeInterval := total / len(nodes)
	for i := 0; i < len(nodes); i++ {
		nas := []corev1.EndpointAddress{}
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

func (s *ServiceLocation) get() (*Location, error) {
	endpoints, err := s.ep.Get(s.cfg.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get endpoints")
	}
	subset := endpoints.Subsets[0]
	as := subset.Addresses
	if len(as) == 0 {
		return &Location{
			Unavailable: true,
		}, nil
	}
	var a *corev1.EndpointAddress
	if !sha1R.Match([]byte(s.params.InfoHash)) {
		a = &as[rand.Intn(len(as))]
	} else if s.cfg.Distribution == HASH {
		a, err = s.distributeByHash(as)
	} else if s.cfg.Distribution == NODE_HASH {
		a, err = s.distributeByNodeHash(as)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to distribute")
	}
	if a != nil && s.nn != "" && *a.NodeName != s.nn && s.cfg.PreferLocalNode {
		las := []corev1.EndpointAddress{}
		for _, a := range as {
			if *a.NodeName == s.nn {
				las = append(las, a)
			}
		}
		if len(las) > 0 {
			a, err = s.distributeByHash(las)
			if err != nil {
				return nil, errors.Wrap(err, "failed to distribute locally")
			}
		}
	}
	return s.addressToLocation(a, &subset), nil
}

func (s *ServiceLocation) Get(purge bool) (*Location, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if purge {
		s.inited = false
	}
	if s.inited {
		return s.loc, s.err
	}
	s.loc, s.err = s.get()
	s.inited = true
	return s.loc, s.err
}
