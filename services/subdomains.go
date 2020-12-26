package services

import (
	"fmt"
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
)

var hexIPPattern = regexp.MustCompile(`[^\.]*`)

type Subdomains struct {
	res                 []string
	inited              bool
	err                 error
	mux                 sync.Mutex
	k8s                 *K8SClient
	infoHash            string
	redirectPrefix      string
	redirectAddressType string
	jobNamespace        string
	naKey               string
	naVal               string
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

func NewSubdomains(c *cli.Context, k8s *K8SClient, infoHash string) *Subdomains {
	return &Subdomains{
		k8s:                 k8s,
		jobNamespace:        c.String(JOB_NAMESPACE),
		naKey:               c.String(JOB_NODE_AFFINITY_KEY),
		naVal:               c.String(JOB_NODE_AFFINITY_VALUE),
		redirectPrefix:      c.String(WEB_ORIGIN_HOST_REDIRECT_PREFIX),
		redirectAddressType: c.String(WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE),
		infoHash:            infoHash,
	}
}

func (s *Subdomains) get() ([]string, error) {
	cl, err := s.k8s.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get k8s client")
	}
	nodeNames := []string{}
	infoHash := s.infoHash
	if infoHash != "" {
		opts := metav1.ListOptions{
			LabelSelector: fmt.Sprintf("info-hash=%v", infoHash),
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
	opts := metav1.ListOptions{}
	if s.naKey != "" && s.naVal != "" && len(nodeNames) == 0 {
		opts.LabelSelector = fmt.Sprintf("%v=%v", s.naKey, s.naVal)
	}
	nodes, err := cl.CoreV1().Nodes().List(opts)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get node client")
	}
	res := []string{}
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Status == corev1.ConditionTrue && c.Type == corev1.NodeReady {
				ready = true
			}
		}
		if !ready {
			continue
		}
		if len(nodeNames) > 0 {
			exist := false
			for _, nn := range nodeNames {
				if nn == n.Name {
					exist = true
				}
			}
			if !exist {
				continue
			}
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeAddressType(s.redirectAddressType) {
				byteIP := net.ParseIP(a.Address)
				hexIP := fmt.Sprintf("%02x%02x%02x%02x", byteIP[12], byteIP[13], byteIP[14], byteIP[15])
				res = append(res, s.redirectPrefix+hexIP)
			}
		}
	}
	sort.Strings(res)
	res2 := []string{}
	if len(nodeNames) == 0 && len(res) > 1 && infoHash != "" {
		hex := infoHash[0:5]
		num, err := strconv.ParseInt(hex, 16, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to parse hex from infohash=%v", infoHash)
		}
		total := 1048575
		interval := int64(total / len(res))
		t := 0
		for i := 0; i < len(res); i++ {
			if num < (int64(i)+1)*interval {
				t = i
				break
			}
		}
		for n := 0; n < 3; n++ {
			m := t + n
			if n >= len(res) {
				m = m - len(res) + 1
			}
			res2 = append(res2, res[m])
		}
	}
	return res2, nil
}

func (s *Subdomains) Get() ([]string, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.res, s.err
	}
	s.res, s.err = s.get()
	s.inited = true
	return s.res, s.err
}
