package services

import (
	"context"
	"github.com/pkg/errors"
	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
	"math/rand"
	"net"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/urfave/cli"
)

var sha1R = regexp.MustCompile("^[0-9a-f]{5,40}$")

type ServiceLocation struct {
	lazymap.LazyMap[*Location]
	ep *K8SEndpoints
	c  *cli.Context
	nn string
}

func NewServiceLocationPool(c *cli.Context, ep *K8SEndpoints) *ServiceLocation {
	return &ServiceLocation{
		c:  c,
		ep: ep,
		nn: c.String(myNodeNameFlag),
		LazyMap: lazymap.New[*Location](&lazymap.Config{
			Expire:      15 * time.Second,
			ErrorExpire: 5 * time.Second,
		}),
	}
}

func (s *ServiceLocation) Get(ctx context.Context, cfg *ServiceConfig, src *Source) (*Location, error) {
	key := cfg.Name + src.InfoHash
	return s.LazyMap.Get(key, func() (*Location, error) {
		return s.get(ctx, cfg, src)
	})
}

func (s *ServiceLocation) get(ctx context.Context, cfg *ServiceConfig, src *Source) (*Location, error) {
	endpoints, err := s.ep.Get(ctx, cfg.Name)
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
	if !sha1R.Match([]byte(src.InfoHash)) {
		a = &as[rand.Intn(len(as))]
	} else if cfg.Distribution == Hash {
		a, err = s.distributeByHash(src, as)
	} else if cfg.Distribution == NodeHash {
		a, err = s.distributeByNodeHash(src, as)
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
		return as[i].IP > as[j].IP
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

func (s *ServiceLocation) distributeByNodeHash(src *Source, as []corev1.EndpointAddress) (*corev1.EndpointAddress, error) {
	sort.Slice(as, func(i, j int) bool {
		return as[i].IP > as[j].IP
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
