package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/allocator"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdsmi"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const mi355xRoot = "../../../testdata/sysfs-mi355x-spx/sys"

type failingPolicy struct{}

func (failingPolicy) Init([]*allocator.Device, string) error { return nil }
func (failingPolicy) Allocate([]string, []string, int) ([]string, error) {
	return nil, errors.New("unknown device id")
}

func TestGetPreferredAllocationEchoesOnAllocatorError(t *testing.T) {
	p := &AMDGPUPlugin{devAllocator: failingPolicy{}}
	resp, err := p.GetPreferredAllocation(context.Background(), &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{{
			AvailableDeviceIDs:   []string{"a#0", "a#1"},
			MustIncludeDeviceIDs: []string{"a#1"},
			AllocationSize:       2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ContainerResponses[0].DeviceIDs; strings.Join(got, ",") != "a#1,a#0" {
		t.Fatalf("DeviceIDs = %v, want [a#1 a#0]", got)
	}
}

func TestNewAMDGPUPluginReadsEnv(t *testing.T) {
	t.Setenv("OPERATING_MODE", "partition")
	t.Setenv("COMPUTE_PARTITION", "qpx")
	if p := NewAMDGPUPlugin(); p.operatingMode != "partition" || p.computePartition != "qpx" {
		t.Fatalf("mode=%q partition=%q", p.operatingMode, p.computePartition)
	}
}

func TestResolveUpstreamAMDGPUIndex(t *testing.T) {
	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"0000:05:00.0": {"card": 1},
			"0000:15:00.0": {"card": 2},
		},
		sortedBDFs:           []string{"0000:05:00.0", "0000:15:00.0", "0000:25:00.0"},
		bdfToROCrUUID:        map[string]string{"0000:05:00.0": "GPU-a", "0000:15:00.0": "GPU-b"},
		amdSMIUUIDToTopology: map[string]string{},
		amdSMIUUIDToROCrUUID: map[string]string{},
		rocrUUIDToTopology:   map[string]string{"GPU-gone": "amdgpu_xcp_9"},
	}
	second := fmt.Sprintf("node-AMDGPU-%d", splitCount+1)
	if d, err := p.deviceDataFromAllocationUUID(second, ""); err != nil || d["card"] != 2 {
		t.Fatalf("device for %s = %v, %v; want card 2", second, d, err)
	}
	if r, err := p.rocrUUIDFromAllocationUUID(second); err != nil || r != "GPU-b" {
		t.Fatalf("ROCr UUID for %s = %q, %v; want GPU-b", second, r, err)
	}
	for _, id := range []string{fmt.Sprintf("node-AMDGPU-%d", 2*splitCount), fmt.Sprintf("node-AMDGPU-%d", 3*splitCount), "node-AMDGPU-x", "GPU-gone", "plain"} {
		if _, err := p.deviceDataFromAllocationUUID(id, ""); err == nil {
			t.Errorf("deviceDataFromAllocationUUID(%q) resolved, want an error", id)
		}
	}
	for _, id := range []string{fmt.Sprintf("node-AMDGPU-%d", 2*splitCount), "node-AMDGPU-x"} {
		if _, err := p.rocrUUIDFromAllocationUUID(id); err == nil {
			t.Errorf("rocrUUIDFromAllocationUUID(%q) resolved, want an error", id)
		}
	}
}

func TestRegistrationWithoutAMDSMI(t *testing.T) {
	fail := func([]string) (map[string]string, error) { return nil, errors.New("amdsmi down") }
	p := NewAMDGPUPlugin(WithSysfsRoot(mi355xRoot), WithAmdSMI(fail, fail, fail),
		WithAMDSPartitionProfiles(func([]string) (map[string][]amdsmi.PartitionProfile, error) {
			return nil, errors.New("amdsmi down")
		}))
	if got := p.getAPIDevices(); len(got) != 0 {
		t.Fatalf("registered %d devices without AMD SMI UUIDs, want 0", len(got))
	}
}

// xcpFixture adds a KFD node for amdgpu_xcp_0 (renderD129, parent
// 0000:75:00.0) to a copy of the MI355X tree, like a kernel exposing XCPs.
func xcpFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(mi355xRoot)); err != nil {
		t.Fatal(err)
	}
	nodes := filepath.Join(root, "class/kfd/kfd/topology/nodes")
	props, err := os.ReadFile(filepath.Join(nodes, "8", "properties"))
	if err != nil {
		t.Fatal(err)
	}
	xcp := strings.Replace(string(props), "drm_render_minor 128", "drm_render_minor 129", 1)
	xcp = strings.Replace(xcp, "unique_id 637966063454657373", "unique_id 42", 1)
	if err := os.MkdirAll(filepath.Join(nodes, "16"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodes, "16", "properties"), []byte(xcp), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestXCPRegistrationReplacesParent(t *testing.T) {
	golden := loadAmdsmiGolden(t, "../../../testdata/amdsmi-mi355x.json")
	lookup := func(field func(amdsmiGolden) string) func([]string) (map[string]string, error) {
		return func(bdfs []string) (map[string]string, error) {
			out := map[string]string{}
			for _, bdf := range bdfs {
				if entry, ok := golden[fixtureBDF(bdf)]; ok {
					out[bdf] = field(entry)
				}
			}
			return out, nil
		}
	}
	profiles := []amdsmi.PartitionProfile{{Type: "SPX", NumPartitions: 1}, {Type: "QPX", NumPartitions: 4}}
	newPlugin := func(mode string) *AMDGPUPlugin {
		p := NewAMDGPUPlugin(WithSysfsRoot(xcpFixture(t)),
			WithAmdSMI(lookup(func(g amdsmiGolden) string { return g.UUID }),
				lookup(func(g amdsmiGolden) string { return g.Type }),
				lookup(func(amdsmiGolden) string { return "nps1" })),
			WithAMDSPartitionProfiles(func(bdfs []string) (map[string][]amdsmi.PartitionProfile, error) {
				out := map[string][]amdsmi.PartitionProfile{}
				for _, bdf := range bdfs {
					out[bdf] = profiles
				}
				return out, nil
			}))
		p.operatingMode = mode
		return p
	}

	devices := newPlugin("partition").getAPIDevices()
	if len(devices) != 8 {
		t.Fatalf("partition mode registered %d devices, want 7 GPUs + 1 XCP: %+v", len(devices), devices)
	}
	var xcp *utils.DeviceInfo
	for _, d := range devices {
		if d.CustomInfo["pciBDF"] == "0000:75:00.0" {
			if xcp != nil {
				t.Fatal("parent 0000:75:00.0 registered alongside its XCP")
			}
			xcp = d
		}
		if bdf, _ := d.CustomInfo["pciBDF"].(string); strings.Count(bdf, ":") != 2 {
			t.Errorf("pciBDF %q is not the standard domain:bus:dev.fn spelling", bdf)
		}
	}
	if xcp == nil || !strings.HasPrefix(xcp.ID, "GPU-") || xcp.Mode != "spx" || xcp.Count != 1 {
		t.Fatalf("XCP entry = %+v, want hard GPU-<id>#spx with Count 1", xcp)
	}
	if xcp.CustomInfo["partitionProfile"] != "spx_nps1" {
		t.Errorf("XCP partitionProfile = %v, want spx_nps1", xcp.CustomInfo["partitionProfile"])
	}

	// cu mode never registers XCPs; the parent stays a whole soft GPU.
	for _, d := range newPlugin("cu").getAPIDevices() {
		if d.Mode != "" || d.Count != int32(splitCount) {
			t.Errorf("cu mode device = %+v, want soft", d)
		}
	}
}

type fakeListAndWatch struct {
	grpc.ServerStream
	sent []*pluginapi.ListAndWatchResponse
}

func (f *fakeListAndWatch) Send(r *pluginapi.ListAndWatchResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func wholeGPUCount() int {
	n := 0
	for key := range amdgpu.GetAMDGPUs() {
		if !strings.HasPrefix(key, "amdgpu_xcp_") {
			n++
		}
	}
	return n
}

// GPUs without a compute partition type stay soft in both modes, so kubelet
// sees splitCount slots per GPU either way.
func TestListAndWatchOnHardware(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("no AMD GPU")
	}
	for _, mode := range []string{"cu", "partition"} {
		p := &AMDGPUPlugin{operatingMode: mode, Heartbeat: make(chan bool), signal: make(chan os.Signal, 1)}
		s := &fakeListAndWatch{}
		done := make(chan error)
		go func() { done <- p.ListAndWatch(&pluginapi.Empty{}, s) }()
		p.Heartbeat <- true
		p.signal <- syscall.SIGTERM
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(s.sent) != 2 {
			t.Fatalf("%s: %d sends, want initial + heartbeat", mode, len(s.sent))
		}
		want := wholeGPUCount() * splitCount
		for _, r := range s.sent {
			if len(r.Devices) != want {
				t.Fatalf("%s: published %d devices, want %d", mode, len(r.Devices), want)
			}
		}
		if got := len(p.getDevices()); got != want {
			t.Fatalf("%s: allocator got %d devices, kubelet %d", mode, got, want)
		}
	}
}

// Start on a real GPU with a fake API server: the node annotation picks the
// mode, the partition flip fails harmlessly on a part without profiles, and
// the register annotation carries the soft GPU.
func TestStartOnHardware(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("no AMD GPU")
	}
	for annotation, wantMode := range map[string]string{"partition": "partition", "Partition": "cu"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node", Annotations: map[string]string{
			operatingModeAnnotation:    annotation,
			computePartitionAnnotation: "qpx",
		}}}
		old := utils.KubeClient
		cs := fake.NewSimpleClientset(node)
		utils.KubeClient = cs
		t.Setenv(utils.NodeNameEnvName, "gpu-node")

		disable := make(chan bool, 1)
		disable <- true // keep WatchAndRegister from re-registering concurrently
		p := NewAMDGPUPlugin(WithAllocator(allocator.NewBestEffortPolicy()))
		p.disableWatchAndRegister = disable
		p.ackDisableWatchAndRegister = make(chan bool, 1)
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		<-p.ackDisableWatchAndRegister
		utils.KubeClient = old

		if p.operatingMode != wantMode || p.computePartition != "qpx" {
			t.Fatalf("annotation %q: mode=%q partition=%q", annotation, p.operatingMode, p.computePartition)
		}
		patched, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu-node", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var registered []utils.DeviceInfo
		if err := json.Unmarshal([]byte(patched.Annotations[registerAnnosKey]), &registered); err != nil {
			t.Fatalf("register annotation %q: %v", patched.Annotations[registerAnnosKey], err)
		}
		if len(registered) != wholeGPUCount() {
			t.Fatalf("registered %d devices, want %d", len(registered), wholeGPUCount())
		}
		for _, d := range registered {
			if d.Mode != "" || d.Count != int32(splitCount) || strings.Count(d.CustomInfo["pciBDF"].(string), ":") != 2 {
				t.Errorf("registered %+v, want a soft GPU with a standard pciBDF", d)
			}
		}
	}
}

func TestNumPartitionsForUnknownType(t *testing.T) {
	if n := numPartitionsForMode([]amdsmi.PartitionProfile{{Type: "SPX", NumPartitions: 1}}, "tpx"); n != 1 {
		t.Fatalf("unknown type = %d partitions, want 1", n)
	}
}

// A GPU whose KFD unique_id is 0 (APU without a Device Serial Number) is
// addressed by ROCr agent index.
func TestRegistrationByROCrIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(mi355xRoot)); err != nil {
		t.Fatal(err)
	}
	props := filepath.Join(root, "class/kfd/kfd/topology/nodes/8/properties")
	b, err := os.ReadFile(props)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(props, []byte(strings.Replace(string(b), "unique_id 637966063454657373", "unique_id 0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	uuid := func(bdfs []string) (map[string]string, error) {
		out := map[string]string{}
		for _, bdf := range bdfs {
			out[bdf] = "uuid-" + bdf
		}
		return out, nil
	}
	p := NewAMDGPUPlugin(WithSysfsRoot(root), WithAmdSMI(uuid, uuid, uuid),
		WithAMDSPartitionProfiles(func([]string) (map[string][]amdsmi.PartitionProfile, error) { return nil, nil }))
	p.getAPIDevices()
	if rocr := p.bdfToROCrUUID["0000:75:00.0"]; rocr != "0" {
		t.Fatalf("ROCr id for the unique_id 0 GPU = %q, want agent index 0", rocr)
	}
}

func TestStartWithoutNode(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("no AMD GPU")
	}
	old := utils.KubeClient
	utils.KubeClient = fake.NewSimpleClientset()
	defer func() { utils.KubeClient = old }()
	t.Setenv(utils.NodeNameEnvName, "missing")
	disable := make(chan bool, 1)
	disable <- true
	p := NewAMDGPUPlugin(WithAllocator(allocator.NewBestEffortPolicy()))
	p.disableWatchAndRegister = disable
	p.ackDisableWatchAndRegister = make(chan bool, 1)
	if err := p.Start(); err == nil {
		t.Fatal("Start without a node object succeeded, want the register error")
	}
	<-p.ackDisableWatchAndRegister
	if p.operatingMode != "cu" {
		t.Fatalf("mode = %q, want the cu default", p.operatingMode)
	}
}
