package services

import (
	"crypto/sha1"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func fakePods(node string, n int, third int) []corev1.EndpointAddress {
	var as []corev1.EndpointAddress
	for i := 0; i < n; i++ {
		name := node
		// Pod IPs 10.233.<third>.<1..n>: string order and numeric order
		// disagree past .9, which is what the old partition sorted by.
		as = append(as, corev1.EndpointAddress{IP: fmt.Sprintf("10.233.%d.%d", third, i+1), NodeName: &name})
	}
	return as
}

func hashes(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("torrent-%d", i)))))
	}
	return out
}

// Removing one pod moves only the torrents that pod held; everything else
// stays where it was. The interval partition moved every torrent between
// the vanished pod's old position and the end of the IP order.
func TestRendezvousOnlyMovesTheLeaversTorrents(t *testing.T) {
	pods := fakePods("worker62", 30, 74)
	before := map[string]string{}
	for _, h := range hashes(3000) {
		before[h] = pickByRendezvous(h, pods).IP
	}
	gone := pods[7].IP
	rest := append(append([]corev1.EndpointAddress{}, pods[:7]...), pods[8:]...)
	moved, held := 0, 0
	for h, ip := range before {
		after := pickByRendezvous(h, rest).IP
		if ip == gone {
			held++
			continue
		}
		if after != ip {
			moved++
		}
	}
	if moved != 0 {
		t.Fatalf("%d torrents of pods that stayed moved; only the %d of the leaver may", moved, held)
	}
	if held == 0 {
		t.Fatal("the leaver held nothing — test is vacuous")
	}
}

// Rendezvous spreads torrents evenly enough: every pod within ±35% of the
// mean over 6000 hashes (interval partition variance is the same order).
func TestRendezvousBalanced(t *testing.T) {
	pods := fakePods("worker62", 30, 74)
	count := map[string]int{}
	n := 6000
	for _, h := range hashes(n) {
		count[pickByRendezvous(h, pods).IP]++
	}
	mean := float64(n) / float64(len(pods))
	for _, p := range pods {
		c := float64(count[p.IP])
		if c < mean*0.65 || c > mean*1.35 {
			t.Fatalf("pod %s got %d torrents, mean %.0f", p.IP, count[p.IP], mean)
		}
	}
}

func TestRendezvousIsDeterministicAndCaseInsensitive(t *testing.T) {
	pods := fakePods("worker62", 30, 74)
	for _, h := range hashes(200) {
		a := pickByRendezvous(h, pods)
		shuffled := append(append([]corev1.EndpointAddress{}, pods[15:]...), pods[:15]...)
		b := pickByRendezvous(h, shuffled)
		c := pickByRendezvous(strings.ToUpper(h), pods)
		if a.IP != b.IP || a.IP != c.IP {
			t.Fatalf("hash %s: %s / %s / %s", h, a.IP, b.IP, c.IP)
		}
	}
}

// The node an infohash lands on is the interval partition rest-api mirrors
// (services/subdomains.go): nodes sorted by name, hash space cut into
// len(nodes) equal intervals from the first five hex digits.
func referenceNode(infohash string, nodes []string) string {
	var num int
	fmt.Sscanf(infohash[0:5], "%x", &num)
	num *= 1000
	total := 1048575 * 1000
	interval := total / len(nodes)
	for i := range nodes {
		if num < (i+1)*interval {
			return nodes[i]
		}
	}
	return nodes[len(nodes)-1]
}

func TestNodeHashKeepsRestAPIPartition(t *testing.T) {
	var as []corev1.EndpointAddress
	as = append(as, fakePods("worker62", 30, 62)...)
	as = append(as, fakePods("worker63", 30, 63)...)
	as = append(as, fakePods("worker64", 30, 64)...)
	s := &ServiceLocation{}
	for _, h := range hashes(2000) {
		a, err := s.distributeByNodeHash(&Source{InfoHash: h}, as, nil)
		if err != nil || a == nil {
			t.Fatalf("hash %s: %v %v", h, a, err)
		}
		if want := referenceNode(h, []string{"worker62", "worker63", "worker64"}); *a.NodeName != want {
			t.Fatalf("hash %s: node %s, rest-api expects %s", h, *a.NodeName, want)
		}
	}
}

// "fffff…" sits at the top of the space; with 7 nodes the floored interval
// leaves it above len*interval and the old loop returned no address.
func TestTopOfHashSpaceLandsOnTheLastNode(t *testing.T) {
	var as []corev1.EndpointAddress
	nodes := []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"}
	for i, n := range nodes {
		as = append(as, fakePods(n, 3, 10+i)...)
	}
	s := &ServiceLocation{}
	top := "fffffffffffffffffffffffffffffffffffffffe"
	a, err := s.distributeByNodeHash(&Source{InfoHash: top}, as, nil)
	if err != nil || a == nil {
		t.Fatalf("top hash: %v %v", a, err)
	}
	if *a.NodeName != "n7" {
		t.Fatalf("top hash landed on %s, want n7", *a.NodeName)
	}
	if b, _ := s.distributeByHash(&Source{InfoHash: top}, as); b == nil {
		t.Fatal("distributeByHash returned no address for the top hash")
	}
}
