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

// Shared with rest-api (services/subdomains_test.go): the rendezvous order
// of nodes for twelve infohashes, over the three workers and over seven
// nodes. Both services must reproduce it literally — the first entry is
// the node thp routes to and rest-api sends the client to.
var rendezvousVector = []struct {
	hash  string
	three []string
	seven []string
}{
	{"06ab54879d3c8177a1fb822437e95842ec3676c2", []string{"worker63", "worker62", "worker64"}, []string{"n1", "n2", "n4", "n3", "n5", "n6", "n7"}},
	{"a7c8d900b2c0a939f4761a3d037fc72384552741", []string{"worker62", "worker63", "worker64"}, []string{"n4", "n5", "n3", "n2", "n6", "n1", "n7"}},
	{"7fe363e26a18b3f3a35226076826b1deef1f18eb", []string{"worker64", "worker62", "worker63"}, []string{"n6", "n4", "n1", "n7", "n2", "n5", "n3"}},
	{"464c43863b3e5ee59ce41bd64851dc53c662949a", []string{"worker64", "worker62", "worker63"}, []string{"n1", "n6", "n4", "n3", "n5", "n2", "n7"}},
	{"cba4525d3a64c5f2421f23b9eb99fe3630538fa1", []string{"worker62", "worker64", "worker63"}, []string{"n4", "n5", "n3", "n6", "n2", "n1", "n7"}},
	{"331522cead8069425d99a9784e11f202a01c0e01", []string{"worker64", "worker62", "worker63"}, []string{"n3", "n4", "n6", "n5", "n1", "n2", "n7"}},
	{"17c08f81c3614734f7ae21a35920e5d443c3060c", []string{"worker63", "worker64", "worker62"}, []string{"n1", "n6", "n4", "n5", "n2", "n7", "n3"}},
	{"4a849030d6bd024394540939e71314db8d136a9e", []string{"worker62", "worker63", "worker64"}, []string{"n5", "n2", "n6", "n4", "n1", "n3", "n7"}},
	{"e831f36d0dd5794b712fe61644efe9bb205c9549", []string{"worker62", "worker64", "worker63"}, []string{"n6", "n4", "n1", "n2", "n3", "n5", "n7"}},
	{"f67dd54947fe381fe0d7359e634aa57ae82324c0", []string{"worker63", "worker64", "worker62"}, []string{"n5", "n2", "n1", "n6", "n7", "n4", "n3"}},
	{"afb1be62a2f9a9bbe3888e81dee9f18e99e064ac", []string{"worker63", "worker62", "worker64"}, []string{"n3", "n7", "n5", "n1", "n6", "n4", "n2"}},
	{"88dd13204862d23f421c2df7bbf50a8b1c729e3d", []string{"worker63", "worker64", "worker62"}, []string{"n4", "n1", "n5", "n7", "n3", "n6", "n2"}},
}

func TestRendezvousOrderMatchesSharedVector(t *testing.T) {
	three := []string{"worker64", "worker62", "worker63"} // deliberately unsorted
	seven := []string{"n7", "n1", "n6", "n2", "n5", "n3", "n4"}
	for _, v := range rendezvousVector {
		if got := rendezvousOrder(v.hash, three); strings.Join(got, ",") != strings.Join(v.three, ",") {
			t.Errorf("%s over 3: got %v want %v", v.hash[:8], got, v.three)
		}
		if got := rendezvousOrder(v.hash, seven); strings.Join(got, ",") != strings.Join(v.seven, ",") {
			t.Errorf("%s over 7: got %v want %v", v.hash[:8], got, v.seven)
		}
	}
}

// NodeHash routes to the vector's first node, then to a pod of that node.
func TestNodeHashRoutesToTheRendezvousOwner(t *testing.T) {
	var as []corev1.EndpointAddress
	as = append(as, fakePods("worker62", 30, 62)...)
	as = append(as, fakePods("worker63", 30, 63)...)
	as = append(as, fakePods("worker64", 30, 64)...)
	s := &ServiceLocation{}
	for _, v := range rendezvousVector {
		a, err := s.distributeByNodeHash(&Source{InfoHash: v.hash}, as, nil)
		if err != nil || a == nil {
			t.Fatalf("hash %s: %v %v", v.hash[:8], a, err)
		}
		if *a.NodeName != v.three[0] {
			t.Fatalf("hash %s: node %s, want %s", v.hash[:8], *a.NodeName, v.three[0])
		}
	}
}

// Losing a node moves only its own hashes; the others keep their node.
func TestNodeRendezvousOnlyMovesTheLostNodesHashes(t *testing.T) {
	nodes := []string{"worker62", "worker63", "worker64", "worker65"}
	rest := []string{"worker62", "worker63", "worker65"}
	moved, lost := 0, 0
	for _, h := range hashes(3000) {
		before := rendezvousPick(h, nodes)
		if before == "worker64" {
			lost++
			continue
		}
		if rendezvousPick(h, rest) != before {
			moved++
		}
	}
	if moved != 0 || lost == 0 {
		t.Fatalf("moved=%d lost=%d", moved, lost)
	}
}

// A hash at the top of the space used to fall out of the interval loop
// into a nil address; every hash has an owner now.
func TestTopOfHashSpaceGetsAnAddress(t *testing.T) {
	var as []corev1.EndpointAddress
	for i, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"} {
		as = append(as, fakePods(n, 3, 10+i)...)
	}
	s := &ServiceLocation{}
	for _, top := range []string{"fffffffffffffffffffffffffffffffffffffffe", "0000000000000000000000000000000000000000"} {
		if a, err := s.distributeByNodeHash(&Source{InfoHash: top}, as, nil); err != nil || a == nil {
			t.Fatalf("%s: %v %v", top[:5], a, err)
		}
		if b, _ := s.distributeByHash(&Source{InfoHash: top}, as); b == nil {
			t.Fatalf("%s: distributeByHash returned no address", top[:5])
		}
	}
}

// preferLocalNode is a locality trick for the viewer's own requests: the
// client was sent to this node, so the local pod holds its state. An
// internal caller (a service on a random node) must keep the rendezvous
// pick — that is the pod the viewer's session lives on.
func TestPreferLocalNodeIsForExternalCallersOnly(t *testing.T) {
	var as []corev1.EndpointAddress
	as = append(as, fakePods("worker62", 30, 62)...)
	as = append(as, fakePods("worker63", 30, 63)...)
	s := &ServiceLocation{nn: "worker63"}
	cfg := &ServiceConfig{PreferLocalNode: true}
	v := rendezvousVector[0]
	picked, err := s.distributeByNodeHash(&Source{InfoHash: v.hash}, as, nil)
	if err != nil || picked == nil {
		t.Fatal(err)
	}
	if *picked.NodeName == "worker63" {
		// pick a hash whose owner is the other node so the override has something to do
		for _, vv := range rendezvousVector {
			p, _ := s.distributeByNodeHash(&Source{InfoHash: vv.hash}, as, nil)
			if p != nil && *p.NodeName != "worker63" {
				v, picked = vv, p
				break
			}
		}
	}
	ext, err := s.preferLocal(cfg, &Source{InfoHash: v.hash}, picked, as)
	if err != nil || ext == nil || *ext.NodeName != "worker63" {
		t.Fatalf("external caller must be moved to the local node, got %v", ext)
	}
	in, err := s.preferLocal(cfg, &Source{InfoHash: v.hash, Internal: true}, picked, as)
	if err != nil || in == nil || in.IP != picked.IP {
		t.Fatalf("internal caller must keep the rendezvous pod %s, got %v", picked.IP, in)
	}
}
