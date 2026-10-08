package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
)

// The MI210 fixture is eight GPUs (KFD nodes 2-9) where each reaches three
// others over XGMI and the rest over PCIe, plus two CPU nodes.
func mi210BDFs() map[int]string {
	m := map[int]string{}
	for n := 2; n <= 9; n++ {
		m[n] = fmt.Sprintf("0000:%02x:00.0", n+0x10)
	}
	return m
}

func TestXGMIPeersOnTheMI210Fixture(t *testing.T) {
	dir := "../../../testdata/topo-mi210-xgmi-pcie/nodes"
	bdfs := mi210BDFs()
	reach := map[int]map[string]bool{}
	for n := 2; n <= 9; n++ {
		peers := xgmiPeers(dir, n, bdfs)
		if len(peers) != 3 {
			t.Fatalf("node %d has %d XGMI peers %v, want 3", n, len(peers), peers)
		}
		reach[n] = map[string]bool{}
		for _, p := range peers {
			reach[n][p] = true
			if p == bdfs[n] {
				t.Fatalf("node %d lists itself", n)
			}
		}
	}
	// A link is reported from both ends.
	for a := 2; a <= 9; a++ {
		for b := 2; b <= 9; b++ {
			if reach[a][bdfs[b]] != reach[b][bdfs[a]] {
				t.Fatalf("link %d-%d is not symmetric", a, b)
			}
		}
	}
}

func TestXGMIPeersLeavesOutUnregisteredAndCPUNodes(t *testing.T) {
	dir := "../../../testdata/topo-mi210-xgmi-pcie/nodes"
	only := map[int]string{4: "0000:14:00.0", 2: "0000:12:00.0"} // node 4 registered with one other GPU
	got := xgmiPeers(dir, 4, only)
	if !reflect.DeepEqual(got, []string{"0000:12:00.0"}) {
		t.Fatalf("peers = %v, want only the registered XGMI neighbour", got)
	}
	if got := xgmiPeers(dir, 0, mi210BDFs()); got != nil {
		t.Fatalf("a CPU node has XGMI peers %v", got)
	}
}

func TestXGMIPeersWithoutLinks(t *testing.T) {
	if got := xgmiPeers(t.TempDir(), 1, map[int]string{1: "0000:06:00.0"}); got != nil {
		t.Fatalf("no topology = %v, want nil", got)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "1", "io_links", "0")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	// PCIe only, and a malformed file next to it.
	if err := os.WriteFile(filepath.Join(link, "properties"), []byte("type 2\nnode_from 1\nnode_to 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "1", "io_links", "1")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "properties"), []byte("garbage\ntype x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := xgmiPeers(dir, 1, map[int]string{0: "0000:00:00.0", 1: "0000:06:00.0"}); got != nil {
		t.Fatalf("PCIe-only node = %v, want nil", got)
	}
}

func TestAddXGMIPeers(t *testing.T) {
	dir := "../../../testdata/topo-mi210-xgmi-pcie/nodes"
	var devices []*utils.DeviceInfo
	var nodes []int
	for n, bdf := range mi210BDFs() {
		devices = append(devices, &utils.DeviceInfo{ID: bdf, CustomInfo: map[string]any{"pciBDF": bdf}})
		nodes = append(nodes, n)
	}
	// A GPU without a topology node must not break the others.
	devices = append(devices, &utils.DeviceInfo{ID: "x", CustomInfo: map[string]any{"pciBDF": "0000:ff:00.0"}})
	nodes = append(nodes, 99)
	addXGMIPeers(devices, nodes, dir)
	for _, d := range devices[:8] {
		peers, ok := d.CustomInfo["xgmiPeers"].([]string)
		if !ok || len(peers) != 3 {
			t.Fatalf("%s xgmiPeers = %v, want 3 peers", d.ID, d.CustomInfo["xgmiPeers"])
		}
	}
	if _, has := devices[8].CustomInfo["xgmiPeers"]; has {
		t.Fatal("a GPU without topology got peers")
	}
	// A single GPU with no links stays unannotated.
	single := []*utils.DeviceInfo{{ID: "a", CustomInfo: map[string]any{"pciBDF": "0000:06:00.0"}}}
	addXGMIPeers(single, []int{1}, t.TempDir())
	if _, has := single[0].CustomInfo["xgmiPeers"]; has {
		t.Fatal("a GPU without links got peers")
	}
}

// In CPX mode every XCP partition is its own KFD node and links over XGMI to
// all others, including the partitions of its own GPU. The peers must be the
// other GPUs' BDFs, not the partitions of the same GPU.
func TestAddXGMIPeersOnCPXPartitions(t *testing.T) {
	dir := "../../../testdata/topo-mi300-cpx/topology/nodes"
	buses := []int{0x05, 0x27, 0x47, 0x65, 0x85, 0xa7, 0xc7, 0xe5}
	var devices []*utils.DeviceInfo
	var nodes []int
	for n := 2; n <= 64; n++ {
		bdf := fmt.Sprintf("0000:%02x:00.0", buses[(n-2)/8])
		devices = append(devices, &utils.DeviceInfo{ID: fmt.Sprint(n), CustomInfo: map[string]any{"pciBDF": bdf}})
		nodes = append(nodes, n)
	}
	addXGMIPeers(devices, nodes, dir)
	for _, d := range devices {
		peers, _ := d.CustomInfo["xgmiPeers"].([]string)
		if len(peers) != 7 {
			t.Fatalf("partition node %s xgmiPeers = %v, want the 7 other GPUs", d.ID, peers)
		}
		for _, p := range peers {
			if p == d.CustomInfo["pciBDF"] {
				t.Fatalf("partition node %s lists its own GPU %s", d.ID, p)
			}
		}
	}
}
