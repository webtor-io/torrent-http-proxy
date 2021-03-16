package services

import (
	"context"
	"fmt"
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
	NODE_HIGH_CPU       = "node-high-cpu"
	NODE_LOW_CPU        = "node-low-cpu"
	NODE_HIGH_BANDWIDTH = "node-high-bandwidth"
	NODE_LOW_BANDWIDTH  = "node-low-bandwidth"
	NODE_NETWORK_IFACE  = "node-netowrk-iface"
)

func RegisterNodesStatFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.Uint64Flag{
		Name:   NODE_HIGH_CPU,
		Usage:  "node high cpu watermark (milli cpus)",
		EnvVar: "NODE_HIGH_CPU",
		Value:  3900,
	})
	c.Flags = append(c.Flags, cli.Uint64Flag{
		Name:   NODE_LOW_CPU,
		Usage:  "node low cpu watermark (milli cpus)",
		EnvVar: "NODE_LOW_CPU",
		Value:  3500,
	})
	c.Flags = append(c.Flags, cli.Uint64Flag{
		Name:   NODE_HIGH_BANDWIDTH,
		Usage:  "node high bandwidth watermark",
		EnvVar: "NODE_HIGH_BANDWIDTH",
		Value:  190 * 1000 * 1000, // 190Mbps
	})
	c.Flags = append(c.Flags, cli.Uint64Flag{
		Name:   NODE_LOW_BANDWIDTH,
		Usage:  "node low bandwidth watermark",
		EnvVar: "NODE_LOW_BANDWIDTH",
		Value:  95 * 1000 * 1000, // 95Mbps
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   NODE_NETWORK_IFACE,
		Usage:  "node network interface",
		EnvVar: "NODE_NETWORK_IFACE",
		Value:  "eth0",
	})
}

type NodeBandwidth struct {
	High    uint64
	Low     uint64
	Current uint64
}
type NodeCPU struct {
	High    uint64
	Low     uint64
	Current uint64
}

type NodeStat struct {
	Name string
	IP   string
	NodeBandwidth
	NodeCPU
	Pool string
}

type NodesStat struct {
	mux     sync.Mutex
	pcl     *PromClient
	kcl     *K8SClient
	l       *logrus.Entry
	stats   []NodeStat
	err     error
	inited  bool
	cpuHigh uint64
	cpuLow  uint64
	bwHigh  uint64
	bwLow   uint64
	iface   string
	raType  string
}

func NewNodesStat(c *cli.Context, pcl *PromClient, kcl *K8SClient, l *logrus.Entry) *NodesStat {
	return &NodesStat{
		pcl:     pcl,
		kcl:     kcl,
		cpuHigh: c.Uint64(NODE_HIGH_CPU),
		cpuLow:  c.Uint64(NODE_LOW_CPU),
		bwHigh:  c.Uint64(NODE_HIGH_BANDWIDTH),
		bwLow:   c.Uint64(NODE_LOW_BANDWIDTH),
		iface:   c.String(NODE_NETWORK_IFACE),
		raType:  c.String(WEB_ORIGIN_HOST_REDIRECT_ADDRESS_TYPE),
	}
}

func (s *NodesStat) get() ([]NodeStat, error) {
	ns, err := s.getKubeStats()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get stats from k8s")
	}
	fmt.Printf("%+v", ns)
	ps, err := s.getPromStats(ns)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get stats from k8s")
	}
	if ps == nil {
		return ns, nil
	}
	return ps, nil
}

func (s *NodesStat) getKubeStats() ([]NodeStat, error) {
	cl, err := s.kcl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get k8s client")
	}
	timeout := int64(5)
	opts := metav1.ListOptions{
		TimeoutSeconds: &timeout,
	}
	nodes, err := cl.CoreV1().Nodes().List(opts)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get nodes")
	}
	res := []NodeStat{}
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
		cpuHigh := s.cpuHigh
		cpuLow := s.cpuLow
		if v, ok := n.GetLabels()["bw-high"]; ok {
			bwHigh, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse bw-high")
			}
		}
		if v, ok := n.GetLabels()["bw-low"]; ok {
			bwLow, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse bw-low")
			}
		}
		if v, ok := n.GetLabels()["cpu-high"]; ok {
			cpuHigh, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse cpu-high")
			}
		}
		if v, ok := n.GetLabels()["cpu-low"]; ok {
			cpuLow, err = bytefmt.ToBytes(v)
			if err != nil {
				return nil, errors.Wrap(err, "Failed to parse cpu-low")
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
			Pool: n.GetLabels()["pool"],
		})
	}
	return res, nil
}

func (s *NodesStat) getPromStats(ns []NodeStat) ([]NodeStat, error) {
	cl, err := s.pcl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get prometheus client")
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
		return nil, errors.Errorf("Failed to parse response %v", val)
	}
	for _, d := range data {
		for i, n := range ns {
			if string(d.Metric["node"]) == n.Name {
				ns[i].NodeBandwidth.Current = uint64(d.Value)
			}
		}
	}
	query = "sum by (instance) (irate(node_cpu_seconds_total{mode!=\"idle\"}[5m])) * on(instance) group_left(nodename) (node_uname_info) * 1000"
	val, _, err = cl.Query(ctx, query, time.Now())
	if err != nil {
		return nil, err
	}
	data, ok = val.(model.Vector)
	if !ok {
		return nil, errors.Errorf("Failed to parse response %v", val)
	}
	for _, d := range data {
		for i, n := range ns {
			if n.Name == string(d.Metric["nodename"]) {
				ns[i].NodeCPU.Current = uint64(d.Value)
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
