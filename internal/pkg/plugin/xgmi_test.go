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
	gpus := map[string]map[string]interface{}{}
	var devices []*utils.DeviceInfo
	for n, bdf := range mi210BDFs() {
		gpus[bdf] = map[string]interface{}{"nodeId": n}
		devices = append(devices, &utils.DeviceInfo{ID: bdf, CustomInfo: map[string]any{"pciBDF": bdf}})
	}
	// A GPU the plugin does not know about must not break the others.
	devices = append(devices, &utils.DeviceInfo{ID: "x", CustomInfo: map[string]any{"pciBDF": "0000:ff:00.0"}})
	addXGMIPeers(devices, gpus, dir)
	for _, d := range devices[:8] {
		peers, ok := d.CustomInfo["xgmiPeers"].([]string)
		if !ok || len(peers) != 3 {
			t.Fatalf("%s xgmiPeers = %v, want 3 peers", d.ID, d.CustomInfo["xgmiPeers"])
		}
	}
	if _, has := devices[8].CustomInfo["xgmiPeers"]; has {
		t.Fatal("an unknown GPU got peers")
	}
	// A single GPU with no links stays unannotated.
	single := []*utils.DeviceInfo{{ID: "a", CustomInfo: map[string]any{"pciBDF": "0000:06:00.0"}}}
	addXGMIPeers(single, map[string]map[string]interface{}{"0000:06:00.0": {"nodeId": 1}}, t.TempDir())
	if _, has := single[0].CustomInfo["xgmiPeers"]; has {
		t.Fatal("a GPU without links got peers")
	}
}
