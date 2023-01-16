package services

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nodeHighBandwidthFlag = "node-high-bandwidth"
	nodeLowBandwidthFlag  = "node-low-bandwidth"
	nodeNetworkIfaceFlag  = "node-netowrk-iface"
)

func RegisterNodesStatFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.Uint64Flag{
			Name:   nodeHighBandwidthFlag,
			Usage:  "node high bandwidth watermark",
			EnvVar: "NODE_HIGH_BANDWIDTH",
			Value:  100 * 1000 * 1000, // 100Mbps
		},
		cli.Uint64Flag{
			Name:   nodeLowBandwidthFlag,
			Usage:  "node low bandwidth watermark",
			EnvVar: "NODE_LOW_BANDWIDTH",
			Value:  85 * 1000 * 1000, // 85Mbps
		},
		cli.StringFlag{
			Name:   nodeNetworkIfaceFlag,
			Usage:  "node network interface",
			EnvVar: "NODE_NETWORK_IFACE",
			Value:  "eth0",
		},
	)
}

type NodeBandwidth struct {
	High    uint64
	Low     uint64
	Current uint64
}
type NodeCPU struct {
	High    float64
	Low     float64
	Current float64
}

type NodeStat struct {
	Name string
	IP   string
	NodeBandwidth
	NodeCPU
	Pool []string
}

type NodesStat struct {
	mux     sync.Mutex
	pcl     *PromClient
	kcl     *K8SClient
	l       *logrus.Entry
	stats   []NodeStat
	err     error
	inited  bool
	cpuHigh float64
	cpuLow  float64
	bwHigh  uint64
	bwLow   uint64
	iface   string
	raType  string
}

func NewNodesStat(c *cli.Context, pcl *PromClient, kcl *K8SClient) *NodesStat {
	return &NodesStat{
		pcl:    pcl,
		kcl:    kcl,
		bwHigh: c.Uint64(nodeHighBandwidthFlag),
		bwLow:  c.Uint64(nodeLowBandwidthFlag),
		iface:  c.String(nodeNetworkIfaceFlag),
		raType: c.String(webOriginHostRedirectAddressTypeFlag),
	}
}

func (s *NodesStat) get() ([]NodeStat, error) {
	ns, err := s.getKubeStats()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get stats from k8s")
	}
	ps, err := s.getPromStats(ns)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get stats from prom")
	}
	if ps == nil {
		return ns, nil
	}
	return ps, nil
}

func parseCPUTime(t string) (float64, error) {
	d := float64(1)
	if strings.HasSuffix(t, "m") {
		d = 1000
		t = strings.TrimSuffix(t, "m")
	}
	v, err := strconv.Atoi(t)
	if err != nil {
		return 0, err
	}
	return float64(v) / d, nil
}

func (s *NodesStat) getKubeStats() ([]NodeStat, error) {
	cl, err := s.kcl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s client")
	}
	timeout := int64(5)
	opts := metav1.ListOptions{
		TimeoutSeconds: &timeout,
	}
	nodes, err := cl.CoreV1().Nodes().List(opts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get nodes")
	}
	var res []NodeStat
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
		ip := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeAddressType(s.raType) {
				ip = a.Address
			}
		}
		bwHigh := s.bwHigh
		bwLow := s.bwLow
		a := n.Status.Allocatable[corev1.ResourceCPU]
		cpuHigh, err := parseCPUTime(a.String())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse allocateble cpu value=%v", a.String())
		}
		cpuLow := cpuHigh - 1
		if v, ok := n.GetLabels()[fmt.Sprintf("%vbandwidth-high", k8SLabelPrefix)]; ok {
			bwHigh, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse bandwidth-high value=%v", v)
			}
		}
		if v, ok := n.GetLabels()[fmt.Sprintf("%vbandwidth-low", k8SLabelPrefix)]; ok {
			bwLow, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse bandwidth-low value=%v", v)
			}
		}
		if v, ok := n.GetLabels()[fmt.Sprintf("%vcpu-high", k8SLabelPrefix)]; ok {
			cpuHigh, err = parseCPUTime(v)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse cpu-high value=%v", v)
			}
		}
		if v, ok := n.GetLabels()[fmt.Sprintf("%vcpu-low", k8SLabelPrefix)]; ok {
			cpuLow, err = parseCPUTime(v)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse cpu-low value=%v", v)
			}
		}
		var pools []string
		for k, v := range n.GetLabels() {
			if strings.HasPrefix(k, k8SLabelPrefix) && strings.HasSuffix(k, "pool") && v == "true" {
				pools = append(pools, strings.TrimSuffix(strings.TrimPrefix(k, k8SLabelPrefix), "-pool"))
			}
		}
		res = append(res, NodeStat{
			Name: n.Name,
			IP:   ip,
			NodeBandwidth: NodeBandwidth{
				High: bwHigh,
				Low:  bwLow,
			},
			NodeCPU: NodeCPU{
				High: cpuHigh,
				Low:  cpuLow,
			},
			Pool: pools,
		})
	}
	return res, nil
}

func (s *NodesStat) getPromStats(ns []NodeStat) ([]NodeStat, error) {
	cl, err := s.pcl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get prometheus client")
	}
	if cl == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	query := fmt.Sprintf("rate(node_network_transmit_bytes_total{device=\"%v\"}[5m]) * on (pod) group_right kube_pod_info * 8", s.iface)
	val, _, err := cl.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}
	data, ok := val.(model.Vector)
	if !ok {
		return nil, errors.Errorf("failed to parse response %v", val)
	}
	for _, d := range data {
		for i, n := range ns {
			if string(d.Metric["node"]) == n.Name {
				ns[i].NodeBandwidth.Current = uint64(d.Value)
			}
		}
	}
	query = "sum by (instance) (irate(node_cpu_seconds_total{mode!=\"idle\"}[5m])) * on(instance) group_left(nodename) (node_uname_info)"
	val, _, err = cl.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}
	data, ok = val.(model.Vector)
	if !ok {
		return nil, errors.Errorf("failed to parse response %v", val)
	}
	for _, d := range data {
		for i, n := range ns {
			if n.Name == string(d.Metric["nodename"]) {
				ns[i].NodeCPU.Current = float64(d.Value)
			}
		}
	}
	return ns, nil
}

func (s *NodesStat) Get() ([]NodeStat, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.stats, s.err
	}
	s.stats, s.err = s.get()
	s.inited = true
	return s.stats, s.err
}
