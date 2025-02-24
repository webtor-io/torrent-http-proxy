package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"github.com/webtor-io/lazymap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nodeLabelPrefixFlag = "node-label-prefix"
)

func RegisterNodesStatFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   nodeLabelPrefixFlag,
			Usage:  "node label prefix",
			EnvVar: "NODE_LABEL_PREFIX",
			Value:  "webtor.io/",
		},
	)
}

type NodeStat struct {
	Name         string
	RolesAllowed []string
	RolesDenied  []string
}

func (s *NodeStat) IsAllowed(role string) bool {
	if len(s.RolesAllowed) > 0 {
		for _, r := range s.RolesAllowed {
			if role == r {
				return true
			}
		}
		return false
	} else if len(s.RolesDenied) > 0 {
		allow := true
		for _, r := range s.RolesDenied {
			if role == r {
				allow = false
			}
		}
		return allow
	}
	return true
}

type NodesStat struct {
	lazymap.LazyMap[[]NodeStat]
	kcl         *K8SClient
	labelPrefix string
}

func NewNodesStat(c *cli.Context, kcl *K8SClient) *NodesStat {
	return &NodesStat{
		LazyMap: lazymap.New[[]NodeStat](&lazymap.Config{
			Concurrency: 1,
			Expire:      60 * time.Second,
			Capacity:    1,
		}),
		kcl:         kcl,
		labelPrefix: c.String(nodeLabelPrefixFlag),
	}
}

func (s *NodesStat) Get(ctx context.Context) ([]NodeStat, error) {
	return s.LazyMap.Get("", func() ([]NodeStat, error) {
		ctx2, cancel := context.WithTimeout(ctx, time.Second*10)
		defer cancel()
		cl, err := s.kcl.Get()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get k8s client")
		}
		nodes, err := cl.CoreV1().Nodes().List(ctx2, metav1.ListOptions{})
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
			res = append(res, NodeStat{
				Name:         n.Name,
				RolesAllowed: s.getLabelList(n, "roles-allowed"),
				RolesDenied:  s.getLabelList(n, "roles-denied"),
			})
		}
		sort.Slice(res, func(i, j int) bool {
			return res[i].Name < res[j].Name
		})
		return res, nil
	})
}

func (s *NodesStat) getLabelList(n corev1.Node, name string) []string {
	var list []string
	if v, ok := n.GetLabels()[fmt.Sprintf("%v%v", s.labelPrefix, name)]; ok {
		list = strings.Split(v, ",")
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
	}
	return list
}
