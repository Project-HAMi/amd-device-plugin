package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	expectedAllLabelKeys = map[string]bool{
		"amd.com/gpu.family":                         true,
		"amd.com/gpu.driver-version":                 true,
		"amd.com/gpu.driver-src-version":             true,
		"amd.com/gpu.firmware":                       true,
		"amd.com/gpu.device-id":                      true,
		"amd.com/gpu.product-name":                   true,
		"amd.com/gpu.vram":                           true,
		"amd.com/gpu.simd-count":                     true,
		"amd.com/gpu.cu-count":                       true,
		"amd.com/gpu.compute-memory-partition":       true,
		"amd.com/gpu.compute-partitioning-supported": true,
		"amd.com/gpu.memory-partitioning-supported":  true,
	}
	expectedAllExperimentalLabelKeys = map[string]bool{
		"beta.amd.com/gpu.family":                         true,
		"beta.amd.com/gpu.driver-version":                 true,
		"beta.amd.com/gpu.driver-src-version":             true,
		"beta.amd.com/gpu.firmware":                       true,
		"beta.amd.com/gpu.device-id":                      true,
		"beta.amd.com/gpu.product-name":                   true,
		"beta.amd.com/gpu.vram":                           true,
		"beta.amd.com/gpu.simd-count":                     true,
		"beta.amd.com/gpu.cu-count":                       true,
		"beta.amd.com/gpu.compute-memory-partition":       true,
		"beta.amd.com/gpu.compute-partitioning-supported": true,
		"beta.amd.com/gpu.memory-partitioning-supported":  true,
	}
)

func TestInitLabelLists(t *testing.T) {
	labelMap := map[string]bool{}
	for _, label := range allLabelKeys {
		labelMap[label] = true
	}
	if !reflect.DeepEqual(labelMap, expectedAllLabelKeys) {
		t.Errorf("failed to get expected all labels during init, got %+v, expect %+v", labelMap, expectedAllLabelKeys)
	}
	experimentalLabelMap := map[string]bool{}
	for _, label := range allExperimentalLabelKeys {
		experimentalLabelMap[label] = true
	}
	if !reflect.DeepEqual(experimentalLabelMap, expectedAllExperimentalLabelKeys) {
		t.Errorf("failed to get expected all experimental labels during init, got %+v, expect %+v", labelMap, expectedAllLabelKeys)
	}
}

func TestRemoveOldNodeLabels(t *testing.T) {
	testCases := []struct {
		inputNode    *corev1.Node
		expectLabels map[string]string
	}{
		{
			inputNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"amd.com/gpu.cu-count":                          "104",
						"amd.com/gpu.device-id":                         "740f",
						"amd.com/gpu.driver-version":                    "6.10.5",
						"amd.com/gpu.family":                            "AI",
						"amd.com/gpu.product-name":                      "Instinct_MI210",
						"amd.com/gpu.simd-count":                        "416",
						"amd.com/gpu.vram":                              "64G",
						"beta.amd.com/gpu.cu-count":                     "104",
						"beta.amd.com/gpu.cu-count.104":                 "1",
						"beta.amd.com/gpu.device-id":                    "740f",
						"beta.amd.com/gpu.device-id.740f":               "1",
						"beta.amd.com/gpu.family":                       "HPC",
						"beta.amd.com/gpu.family.HPC":                   "1",
						"beta.amd.com/gpu.product-name":                 "Instinct_MI300X",
						"beta.amd.com/gpu.product-name.Instinct_MI300X": "1",
						"beta.amd.com/gpu.simd-count":                   "416",
						"beta.amd.com/gpu.simd-count.416":               "1",
						"beta.amd.com/gpu.vram":                         "64G",
						"beta.amd.com/gpu.vram.64G":                     "1",
						"amd.com/gpu.family.AI":                         "1",
						"amd.com/gpu.family.HPC":                        "1",
						"beta.amd.com/gpu.family.AI":                    "1",
						"beta.amd.com/gpu.firmware.SMC.fw.123":          "1",
						"beta.amd.com/gpu.firmware.ME.feat.35":          "1",
						"dummyLabel1":                                   "1",
						"dummyLabel2":                                   "2",
					},
				},
			},
			expectLabels: map[string]string{
				"dummyLabel1": "1",
				"dummyLabel2": "2",
			},
		},
		{
			inputNode: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"amd.com/cpu":    "true",
						"amd.com/gpu":    "true",
						"amd.com/mi300x": "true",
						"dummyLabel1":    "1",
						"dummyLabel2":    "2",
					},
				},
			},
			expectLabels: map[string]string{
				"amd.com/cpu":    "true",
				"amd.com/gpu":    "true",
				"amd.com/mi300x": "true",
				"dummyLabel1":    "1",
				"dummyLabel2":    "2",
			},
		},
	}

	for _, tc := range testCases {
		removeOldNodeLabels(tc.inputNode)
		if !reflect.DeepEqual(tc.inputNode.Labels, tc.expectLabels) {
			t.Errorf("failed to get expected node labels after removing old labels, got %+v, expect %+v", tc.inputNode.Labels, tc.expectLabels)
		}
	}
}

func TestMatchesRenderD(t *testing.T) {
	gpu := map[string]interface{}{"renderD": 128}
	if !matchesRenderD(128, gpu) {
		t.Error("128 should match renderD 128")
	}
	if matchesRenderD(129, gpu) {
		t.Error("129 should not match renderD 128")
	}
	// a missing renderD must not match a failed parse that yields 0
	if matchesRenderD(0, map[string]interface{}{}) {
		t.Error("missing renderD should not match")
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	long := "AMD Radeon Pro W7900 Dual Slot With An Extremely Long Marketing Name"
	for in, want := range map[string]string{
		"AMD Instinct MI300X (OAM)":  "AMD_Instinct_MI300X_OAM",
		"Radeon RX 9060 XT/16GB, OC": "Radeon_RX_9060_XT_16GB__OC",
		" (Radeon) ":                 "Radeon",
		long:                         "AMD_Radeon_Pro_W7900_Dual_Slot_With_An_Extreme",
	} {
		got := sanitizeLabelValue(in, productNameMaxLen)
		if got != want {
			t.Errorf("sanitizeLabelValue(%q) = %q, want %q", in, got, want)
		}
		if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
			t.Errorf("%q is not a valid label value: %v", got, errs)
		}
		if errs := validation.IsQualifiedName("beta.amd.com/gpu.product-name." + got); len(errs) > 0 {
			t.Errorf("key for %q is invalid: %v", got, errs)
		}
	}
}

func TestParseDeviceID(t *testing.T) {
	for in, want := range map[string]string{"0x740f\n": "740f", "740f": "740f", "": "", "7": "7"} {
		if got := parseDeviceID(in); got != want {
			t.Errorf("parseDeviceID(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeTopology writes one KFD topology node per GPU: render minor, SIMD and
// VRAM size, plus a CPU node without a render minor.
func fakeTopology(t *testing.T, nodes map[int][3]int64) {
	t.Helper()
	root := t.TempDir()
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "0", "properties"), "cpu_cores_count 16\nsimd_count 0\n")
	for n, v := range nodes {
		dir := filepath.Join(root, strconv.Itoa(n))
		write(filepath.Join(dir, "properties"), fmt.Sprintf("simd_count %d\nsimd_per_cu 2\ndrm_render_minor %d\n", v[1], v[0]))
		write(filepath.Join(dir, "mem_banks", "0", "properties"), fmt.Sprintf("heap_type 1\nsize_in_bytes %d\n", v[2]))
	}
	old := topologyRoot
	topologyRoot = root
	t.Cleanup(func() { topologyRoot = old })
}

func TestTopologyLabelsFollowEachGPU(t *testing.T) {
	// an RX 9060 XT (64 SIMDs, 16 GiB) on renderD128 and an iGPU on renderD129
	fakeTopology(t, map[int][3]int64{1: {128, 64, 16 << 30}, 2: {129, 4, 512 << 20}})
	gpus := map[string]map[string]interface{}{
		"card0": {"card": 0, "renderD": 128},
		"card1": {"card": 1, "renderD": 129},
	}
	for gen, want := range map[string]map[string]string{
		"cu-count":   {"amd.com/gpu.cu-count.32": "1", "amd.com/gpu.cu-count.2": "1"},
		"simd-count": {"amd.com/gpu.simd-count.64": "1", "amd.com/gpu.simd-count.4": "1"},
		"vram":       {"amd.com/gpu.vram.16G": "1", "amd.com/gpu.vram.1G": "1"},
	} {
		got := labelGenerators[gen](gpus)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q (all: %v)", gen, k, got[k], v, got)
			}
		}
	}
	// a GPU without a matching topology node gets no label
	if got := labelGenerators["cu-count"](map[string]map[string]interface{}{"card9": {"renderD": 200}}); len(got) != 0 {
		t.Errorf("unmatched GPU labelled: %v", got)
	}
}

func TestOwnNodeNameRequiresDownwardAPI(t *testing.T) {
	t.Setenv("DS_NODE_NAME", "")
	if _, err := ownNodeName(); err == nil {
		t.Error("empty DS_NODE_NAME accepted")
	}
	t.Setenv("DS_NODE_NAME", "gpu-1")
	if n, err := ownNodeName(); err != nil || n != "gpu-1" {
		t.Errorf("ownNodeName() = %q, %v", n, err)
	}
}

func TestReconcileReplacesManagedLabels(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{
		"amd.com/gpu.vram":         "8G",
		"beta.amd.com/gpu.vram.8G": "1",
		"team":                     "ml",
	}}}
	c := fake.NewClientBuilder().WithObjects(node).Build()
	r := &reconcileNodeLabels{client: c, log: log, labels: map[string]string{"amd.com/gpu.vram": "16G"}}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "gpu-1"}}); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-1"}, got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"amd.com/gpu.vram": "16G", "team": "ml"}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels = %v, want %v", got.Labels, want)
	}
}
