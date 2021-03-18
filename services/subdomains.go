package services

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE = "origin-host-redirect-address-type"
	WEB_ORIGIN_HOST_REDIRECT              = "origin-host-redirect"
	WEB_ORIGIN_HOST_REDIRECT_PREFIX       = "origin-host-redirect-prefix"
	MAX_SUBDOMAINS                        = 3
	INFOHASH_MAX_SPREAD                   = 1
)

var hexIPPattern = regexp.MustCompile(`[^\.]*`)

type Subdomains struct {
	subs                []string
	sc                  []NodeStatWithScore
	inited              bool
	err                 error
	mux                 sync.Mutex
	k8s                 *K8SClient
	nsp                 *NodesStatPool
	infoHash            string
	redirectPrefix      string
	redirectAddressType string
	jobNamespace        string
	useCPU              bool
	useBandwidth        bool
	pool                string
	skipActiveJobSearch bool
}

func RegisterSubdomainsFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   WEB_ORIGIN_HOST_REDIRECT_PREFIX,
		Usage:  "subdomain prefix of host to be redirected",
		EnvVar: "WEB_ORIGIN_HOST_REDIRECT_PREFIX",
		Value:  "abra--",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE,
		Usage:  "preferred node address type",
		EnvVar: "WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE",
		Value:  "ExternalIP",
	})
}

func NewSubdomains(c *cli.Context, k8s *K8SClient, nsp *NodesStatPool, infoHash string, skipActiveJobSearch bool, useCPU bool, useBandwidth bool, pool string) *Subdomains {
	return &Subdomains{
		k8s:                 k8s,
		nsp:                 nsp,
		jobNamespace:        c.String(JOB_NAMESPACE),
		redirectPrefix:      c.String(WEB_ORIGIN_HOST_REDIRECT_PREFIX),
		redirectAddressType: c.String(WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE),
		infoHash:            infoHash,
		useCPU:              useCPU,
		useBandwidth:        useBandwidth,
		pool:                pool,
		skipActiveJobSearch: skipActiveJobSearch,
	}
}

type NodeStatWithScore struct {
	NodeStat
	Score    float64
	Distance int
}

func (s *Subdomains) filterByPool(stats []NodeStatWithScore) []NodeStatWithScore {
	res := []NodeStatWithScore{}
	for _, st := range stats {
		if s.pool == st.Pool {
			res = append(res, st)
		}
	}
	return res
}

func (s *Subdomains) filterByActivePod(stats []NodeStatWithScore) ([]NodeStatWithScore, error) {
	cl, err := s.k8s.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get k8s client")
	}
	nodeNames := []string{}
	infoHash := s.infoHash
	timeout := int64(5)
	if infoHash != "" {
		opts := metav1.ListOptions{
			LabelSelector:  fmt.Sprintf("info-hash=%v", infoHash),
			TimeoutSeconds: &timeout,
		}
		pods, err := cl.CoreV1().Pods(s.jobNamespace).List(opts)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to find active job")
		}
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodFailed {
				nodeNames = append(nodeNames, p.Spec.NodeName)
			}
		}
	}
	if len(nodeNames) == 0 {
		return stats, nil
	}
	res := []NodeStatWithScore{}
	for _, nn := range nodeNames {
		for _, st := range stats {
			if nn == st.Name {
				res = append(res, st)
			}
		}
	}
	return res, nil
}

func (s *Subdomains) updateScoreByCPU(stats []NodeStatWithScore) []NodeStatWithScore {
	for i, v := range stats {
		if v.NodeCPU.Current < v.NodeCPU.Low {
			continue
		} else if v.NodeCPU.Current >= v.NodeCPU.High {
			stats[i].Score = 0
		} else {
			ratio := float64(v.NodeCPU.High-v.NodeCPU.Current) / float64(v.NodeCPU.High-v.NodeCPU.Low)
			stats[i].Score = stats[i].Score * ratio * ratio
		}
	}
	return stats
}

func (s *Subdomains) updateScoreByInfoHash(stats []NodeStatWithScore) ([]NodeStatWithScore, error) {
	if s.infoHash == "" {
		return stats, nil
	}
	if len(stats) == 0 {
		return stats, nil
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name > stats[j].Name
	})
	hex := s.infoHash[0:5]
	num, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to parse hex from infohash=%v", s.infoHash)
	}
	total := 1048575
	interval := int64(total / len(stats))
	t := 0
	for i := 0; i < len(stats); i++ {
		if num < (int64(i)+1)*interval {
			t = i
			break
		}
	}

	spread := int(math.Floor(float64(len(stats)) / 2))
	if spread > INFOHASH_MAX_SPREAD {
		spread = INFOHASH_MAX_SPREAD
	}
	for i, _ := range stats {
		stats[i].Distance = spread + 1
	}
	for n := -spread; n <= spread; n++ {
		m := t + n
		if m < 0 {
			m = len(stats) + m
		}
		if m >= len(stats) {
			m = m - len(stats)
		}
		d := math.Abs(float64(n))
		stats[m].Distance = int(d)
	}
	for i, _ := range stats {
		if stats[i].Distance == 0 {
			continue
		}
		ratio := 1 / float64(stats[i].Distance) / 2
		stats[i].Score = stats[i].Score * ratio
	}
	return stats, nil
}
func (s *Subdomains) updateScoreByBandwidth(stats []NodeStatWithScore) []NodeStatWithScore {
	for i, v := range stats {
		if v.NodeBandwidth.Current < v.NodeBandwidth.Low {
			continue
		} else if v.NodeBandwidth.Current >= v.NodeBandwidth.High {
			stats[i].Score = 0
		} else {
			ratio := float64(v.NodeBandwidth.High-v.NodeBandwidth.Current) / float64(v.NodeBandwidth.High-v.NodeBandwidth.Low)
			stats[i].Score = stats[i].Score * ratio * ratio
		}
	}
	return stats
}

func (s *Subdomains) getScoredStats() ([]NodeStatWithScore, error) {
	stats, err := s.nsp.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get nodes stat")
	}
	sc := []NodeStatWithScore{}
	for _, s := range stats {
		sc = append(sc, NodeStatWithScore{
			NodeStat: s,
			Score:    1,
			Distance: -1,
		})
	}
	if s.pool != "" {
		sc = s.filterByPool(sc)
	}
	if !s.skipActiveJobSearch {
		sc, err = s.filterByActivePod(sc)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to filter by active job")
		}
	}
	if s.useCPU {
		sc = s.updateScoreByCPU(sc)
	}
	if s.useBandwidth {
		sc = s.updateScoreByBandwidth(sc)
	}
	sc, err = s.updateScoreByInfoHash(sc)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to update score by hash")
	}
	sort.Slice(sc, func(i, j int) bool {
		return sc[i].Score > sc[j].Score
	})
	// fmt.Printf("%+v", sc)
	return sc, nil
}

func (s *Subdomains) get() ([]NodeStatWithScore, []string, error) {
	stats, err := s.getScoredStats()
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to get sorted nodes stat")
	}
	res := []string{}
	for _, st := range stats {
		byteIP := net.ParseIP(st.IP)
		hexIP := fmt.Sprintf("%02x%02x%02x%02x", byteIP[12], byteIP[13], byteIP[14], byteIP[15])
		res = append(res, s.redirectPrefix+hexIP)
	}
	l := len(res)
	if l > MAX_SUBDOMAINS {
		l = MAX_SUBDOMAINS
	}
	return stats, res[0:l], nil
}

func (s *Subdomains) Get() ([]NodeStatWithScore, []string, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.sc, s.subs, s.err
	}
	s.sc, s.subs, s.err = s.get()
	s.inited = true
	return s.sc, s.subs, s.err
}
