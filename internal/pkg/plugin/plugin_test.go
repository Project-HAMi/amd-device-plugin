/**
 * Copyright 2018 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdsmi"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/dmem"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestDeviceDataFromAMDSMIUUID(t *testing.T) {
	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"0000:83:00.0": {"card": 1},
		},
		amdSMIUUIDToTopology: map[string]string{
			"8eff74b5-0000-1000-801b-b56457addd1b": "0000:83:00.0",
		},
		amdSMIUUIDToROCrUUID: map[string]string{
			"8eff74b5-0000-1000-801b-b56457addd1b": "GPU-466450b96fbde849",
		},
	}

	device, err := p.deviceDataFromAllocationUUID("8eff74b5-0000-1000-801b-b56457addd1b")
	if err != nil {
		t.Fatalf("resolve AMD SMI UUID: %v", err)
	}
	if device["card"] != 1 {
		t.Fatalf("resolved device = %#v, want card 1", device)
	}
	rocrUUID, err := p.rocrUUIDFromAllocationUUID("8eff74b5-0000-1000-801b-b56457addd1b")
	if err != nil {
		t.Fatalf("resolve ROCr UUID: %v", err)
	}
	if rocrUUID != "GPU-466450b96fbde849" {
		t.Fatalf("ROCr UUID = %q", rocrUUID)
	}
}

func TestBuildCUAllocationsFromPods(t *testing.T) {
	p := &AMDGPUPlugin{deviceCache: []*utils.DeviceInfo{
		{ID: "node~gpu0", Devcore: 8},
		{ID: "node~gpu1", Devcore: 8},
	}}
	pods := []corev1.Pod{
		makeCUPod("running-a", corev1.PodRunning, `{"node~gpu0":"0-3"}`),
		makeCUPod("running-b", corev1.PodPending, `{"node~gpu0":"4-5","node~gpu1":"0"}`),
		makeCUPod("completed", corev1.PodSucceeded, `{"node~gpu0":"6-7"}`),
		makeCUPod("failed", corev1.PodFailed, `{"node~gpu1":"1-7"}`),
	}

	allocations, err := p.buildCUAllocations(pods)
	if err != nil {
		t.Fatalf("build CU allocations: %v", err)
	}
	assertAllocationWord(t, allocations, "node~gpu0", 0x3f)
	assertAllocationWord(t, allocations, "node~gpu1", 0x01)
}

func TestBuildCUAllocationsRejectsOverlap(t *testing.T) {
	p := &AMDGPUPlugin{deviceCache: []*utils.DeviceInfo{{ID: "node~gpu0", Devcore: 8}}}
	pods := []corev1.Pod{
		makeCUPod("pod-a", corev1.PodRunning, `{"node~gpu0":"0-3"}`),
		makeCUPod("pod-b", corev1.PodRunning, `{"node~gpu0":"3-4"}`),
	}

	_, err := p.buildCUAllocations(pods)
	if err == nil {
		t.Fatal("expected overlapping allocation to fail")
	}
	for _, want := range []string{"node~gpu0", "CU 3", "default/pod-a", "default/pod-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("overlap error %q does not contain %q", err, want)
		}
	}
}

func TestBuildCUAllocationsRejectsOutOfRangeCU(t *testing.T) {
	p := &AMDGPUPlugin{deviceCache: []*utils.DeviceInfo{{ID: "node~gpu0", Devcore: 8}}}
	_, err := p.buildCUAllocations([]corev1.Pod{
		makeCUPod("invalid", corev1.PodRunning, `{"node~gpu0":"8"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("expected out-of-range error, got %v", err)
	}
}

func TestNextAllocationUsesPersistedPodState(t *testing.T) {
	const (
		uuid     = "node~gpu0"
		totalCUs = 8
	)
	p := &AMDGPUPlugin{deviceCache: []*utils.DeviceInfo{{ID: uuid, Devcore: totalCUs}}}

	// This is the only state left after a plugin restart or before any informer
	// event: the first Pod's durable annotation.
	occupied, err := p.buildCUAllocations([]corev1.Pod{
		makeCUPod("first", corev1.PodRunning, `{"node~gpu0":"0-3"}`),
	})
	if err != nil {
		t.Fatalf("rebuild first Pod allocation: %v", err)
	}
	_, delta, err := cuallocation.AllocateN(occupied[uuid], totalCUs, 4, 1)
	if err != nil {
		t.Fatalf("allocate second Pod: %v", err)
	}
	if delta[0] != 0xf0 {
		t.Fatalf("second allocation = %#x, want %#x", delta[0], uint64(0xf0))
	}
}

func makeCUPod(name string, phase corev1.PodPhase, allocation string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			Annotations: map[string]string{
				utils.CuAllocation: allocation,
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func assertAllocationWord(t *testing.T, allocations map[string]cuallocation.Allocation, uuid string, want uint64) {
	t.Helper()
	allocation, ok := allocations[uuid]
	if !ok || len(allocation) == 0 {
		t.Fatalf("allocation for %s is missing", uuid)
	}
	if allocation[0] != want {
		t.Fatalf("allocation word for %s = %#x, want %#x", uuid, allocation[0], want)
	}
}

func TestIsWholeGPU(t *testing.T) {
	p := &AMDGPUPlugin{deviceCache: []*utils.DeviceInfo{
		{ID: "a", Devcore: 32, Devmem: 16304, CustomInfo: map[string]any{"pciBDF": "0000:06:00.0"}},
		{ID: "b", Devcore: 32, Devmem: 16304},
	}}
	for _, tc := range []struct {
		name   string
		devreq utils.ContainerDevices
		want   bool
	}{
		{"fallback zeros", utils.ContainerDevices{{UUID: "a"}, {UUID: "b"}}, true},
		{"scheduler writes full size", utils.ContainerDevices{{UUID: "a", Usedcores: 32, Usedmem: 16304}}, true},
		{"kubelet split id", utils.ContainerDevices{{UUID: "0000:06:00.0#3"}}, true},
		{"core slice", utils.ContainerDevices{{UUID: "a", Usedcores: 16, Usedmem: 16304}}, false},
		{"memory slice", utils.ContainerDevices{{UUID: "a", Usedcores: 32, Usedmem: 4096}}, false},
		{"one sliced of two", utils.ContainerDevices{{UUID: "a"}, {UUID: "b", Usedcores: 20}}, false},
		{"unknown device", utils.ContainerDevices{{UUID: "z"}}, false},
	} {
		if got := p.isWholeGPU(tc.devreq); got != tc.want {
			t.Errorf("%s: isWholeGPU = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// the kubelet device plugin API requires the server to embed UnimplementedDevicePluginServer
var _ dpm.PluginInterface = (*AMDGPUPlugin)(nil)

func TestMarkNonFunctional(t *testing.T) {
	p := &AMDGPUPlugin{AMDGPUs: map[string]map[string]interface{}{
		"0000:06:00.0": {"card": 1},
		"0000:07:00.0": {"card": 2},
	}}
	devs := []*pluginapi.Device{
		{ID: "0000:06:00.0#0", Health: pluginapi.Healthy},
		{ID: "0000:06:00.0#1", Health: pluginapi.Healthy},
		{ID: "0000:07:00.0#0", Health: pluginapi.Healthy},
		{ID: "0000:08:00.0#0", Health: pluginapi.Healthy},
	}
	p.markNonFunctional(devs, func(card string) bool { return card == "card1" })
	want := []string{pluginapi.Healthy, pluginapi.Healthy, pluginapi.Unhealthy, pluginapi.Unhealthy}
	for i, d := range devs {
		if d.Health != want[i] {
			t.Errorf("%s: health %s, want %s", d.ID, d.Health, want[i])
		}
	}
}

func TestComputeQueues(t *testing.T) {
	dir := "../../../testdata/topo-mi300-cpx/topology/nodes"
	if q, ok := computeQueues(dir, 32); !ok || q != 24 {
		t.Errorf("computeQueues(node 32) = %d, %v; want 24, true", q, ok)
	}
	if _, ok := computeQueues(dir, 9999); ok {
		t.Error("missing node should not report compute queues")
	}
}

// gfx12 has only 2 CP pipes for user queues, so it defaults to 2 sharers per
// GPU; an explicit --split_count applies to every GPU.
func TestListerSplitCount(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	dir := t.TempDir()
	for node, gfx := range map[string]string{"1": "120001", "2": "100306", "3": "90402"} {
		if err := os.MkdirAll(filepath.Join(dir, node), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, node, "properties"), []byte("gfx_target_version "+gfx+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	(&AMDGPULister{}).NewPlugin("gpu")
	for node, want := range map[int]int{1: 2, 2: 10, 3: 10, 9: 10} {
		if got := splitCountFor(dir, node); got != want {
			t.Errorf("default split count on node %d = %d, want %d", node, got, want)
		}
	}
	(&AMDGPULister{SplitCount: 4}).NewPlugin("gpu")
	if got := splitCountFor(dir, 1); got != 4 {
		t.Errorf("explicit split count on gfx12 = %d, want 4", got)
	}
}

// A whole-GPU request owns the card, so Allocate needs no node lock and
// persists no CU occupancy; a slice still requires the scheduler's lock.
func TestAllocateWholeGPUSkipsCUCommit(t *testing.T) {
	for _, tc := range []struct {
		name, request string
		wantErr       bool
	}{
		{"scheduler whole gpu", "uuid-a,AMDGPU,16304,32:;", false},
		{"core slice", "uuid-a,AMDGPU,4096,16:;", true},
	} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{
				utils.BindTimeAnnotations:     "1",
				utils.AssignedNodeAnnotations: "n",
				utils.DeviceBindPhase:         utils.DeviceBindAllocating,
				utils.DeviceToAllocate:        tc.request,
			}},
			Spec:   corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Name: "c"}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
		cs := fake.NewSimpleClientset(node, pod)
		old := utils.KubeClient
		t.Cleanup(func() { utils.KubeClient = old })
		utils.KubeClient = cs
		t.Setenv(utils.NodeNameEnvName, "n")

		p := &AMDGPUPlugin{
			AMDGPUs:              map[string]map[string]interface{}{"0000:06:00.0": {"card": 1, "renderD": 129}},
			amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:06:00.0"},
			amdSMIUUIDToROCrUUID: map[string]string{"uuid-a": "GPU-1"},
			deviceCache:          []*utils.DeviceInfo{{ID: "uuid-a", Devcore: 32, Devmem: 16304}},
		}
		resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"0000:06:00.0#0"}}},
		})
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: Allocate error = %v, want error %v", tc.name, err, tc.wantErr)
		}
		// A whole GPU loads no hook, so it must not need one on the node.
		if err == nil && len(resp.ContainerResponses[0].Mounts) != 0 {
			t.Errorf("%s: whole-GPU allocation mounts %v", tc.name, resp.ContainerResponses[0].Mounts)
		}
		got, getErr := cs.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
		if getErr != nil {
			t.Fatal(getErr)
		}
		if _, persisted := got.Annotations[utils.CuAllocation]; persisted {
			t.Errorf("%s: CU occupancy persisted without the node lock", tc.name)
		}
	}
}

func TestIsSchedulableTopologyKey(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	splitCount = 10 // these tests pin one slot count for every GPU
	wholeGPU := map[string]interface{}{"devID": "0000:05:00.0", "computePartitionType": "spx", "memoryPartitionType": "nps1"}
	qpxParent := map[string]interface{}{"devID": "0000:15:00.0", "computePartitionType": "qpx", "memoryPartitionType": "nps1"}
	noPartition := map[string]interface{}{"devID": "0000:75:00.0", "computePartitionType": "", "memoryPartitionType": ""}
	xcp := map[string]interface{}{"devID": "0000:15:00.0", "computePartitionType": "qpx", "memoryPartitionType": "nps1"}
	qpxNoXCP := map[string]interface{}{"devID": "0000:25:00.0", "computePartitionType": "qpx", "memoryPartitionType": "nps1"}
	partitions := xcpParents(map[string]map[string]interface{}{
		"0000:05:00.0": wholeGPU, "0000:15:00.0": qpxParent, "amdgpu_xcp_30": xcp,
		"0000:75:00.0": noPartition, "0000:25:00.0": qpxNoXCP,
	})
	if !(&AMDGPUPlugin{operatingMode: "partition"}).isSchedulableTopologyKey("0000:25:00.0", qpxNoXCP, partitions) {
		t.Error("partition mode: QPX GPU whose kernel exposes no XCP nodes should register whole")
	}

	cu := &AMDGPUPlugin{operatingMode: "cu"}
	if !cu.isSchedulableTopologyKey("0000:05:00.0", wholeGPU, partitions) {
		t.Error("cu mode: SPX whole GPU should be schedulable")
	}
	if !cu.isSchedulableTopologyKey("0000:15:00.0", qpxParent, partitions) {
		t.Error("cu mode: QPX whole GPU should be schedulable (CU slicing over the whole GPU)")
	}
	if cu.isSchedulableTopologyKey("amdgpu_xcp_30", xcp, partitions) {
		t.Error("cu mode: XCP partition must not register")
	}
	if !cu.isSchedulableTopologyKey("0000:75:00.0", noPartition, partitions) {
		t.Error("cu mode: GPU without partition support should be schedulable")
	}

	partition := &AMDGPUPlugin{operatingMode: "partition"}
	if !partition.isSchedulableTopologyKey("0000:05:00.0", wholeGPU, partitions) {
		t.Error("partition mode: SPX whole GPU (1 partition) should register as a hard device")
	}
	if partition.isSchedulableTopologyKey("0000:15:00.0", qpxParent, partitions) {
		t.Error("partition mode: QPX whole GPU is replaced by its XCP partitions")
	}
	if !partition.isSchedulableTopologyKey("amdgpu_xcp_30", xcp, partitions) {
		t.Error("partition mode: XCP partition should register")
	}
	if !partition.isSchedulableTopologyKey("0000:75:00.0", noPartition, partitions) {
		t.Error("partition mode: GPU without partitions has no hard form but still registers soft")
	}
}

// The scheduler (registerHardEntry) and kubelet (kubeletIDs) views must agree:
// a hard entry is exactly one kubelet device, a soft one splitCount slots.
func TestKubeletAndSchedulerViewsAgree(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	splitCount = 10 // these tests pin one slot count for every GPU
	devices := map[string]map[string]interface{}{
		"amdgpu_xcp_30": {"computePartitionType": "cpx"},
		"0000:05:00.0":  {"computePartitionType": "spx"},
		"0000:75:00.0":  {"computePartitionType": ""},
	}
	for _, mode := range []string{"cu", "partition"} {
		p := &AMDGPUPlugin{operatingMode: mode}
		for key, data := range devices {
			info := &utils.DeviceInfo{ID: "uuid", Count: int32(splitCount)}
			hard := p.registerHardEntry(data, info)
			ids := p.kubeletIDs(key, data)
			if int32(len(ids)) != info.Count {
				t.Errorf("%s %s: scheduler Count=%d, kubelet ids=%v", mode, key, info.Count, ids)
			}
			wantHard := mode == "partition" && data["computePartitionType"] != ""
			if hard != wantHard {
				t.Errorf("%s %s: hard=%v, want %v", mode, key, hard, wantHard)
			}
			if hard && (ids[0] != key+"#"+info.Mode || info.ID != "uuid#"+info.Mode) {
				t.Errorf("%s %s: hard ids kubelet=%v scheduler=%s", mode, key, ids, info.ID)
			}
		}
	}
}

func TestEchoPreferred(t *testing.T) {
	got := echoPreferred([]string{"a#0", "a#1", "b#0"}, []string{"a#1"}, 2)
	if strings.Join(got, ",") != "a#1,a#0" {
		t.Fatalf("echoPreferred = %v, want [a#1 a#0]", got)
	}
}

func TestStripPartitionModeSuffix(t *testing.T) {
	for id, want := range map[string]string{
		"GPU-466450b96fbde849#cpx":             "GPU-466450b96fbde849",
		"GPU-466450b96fbde849#spx":             "GPU-466450b96fbde849",
		"GPU-466450b96fbde849#tpx":             "GPU-466450b96fbde849",
		"GPU-466450b96fbde849":                 "GPU-466450b96fbde849",
		"GPU-466450b96fbde849#0":               "GPU-466450b96fbde849#0",
		"8eff74b5-0000-1000-801b-b56457addd1b": "8eff74b5-0000-1000-801b-b56457addd1b",
	} {
		if got := stripPartitionModeSuffix(id); got != want {
			t.Errorf("stripPartitionModeSuffix(%q) = %q, want %q", id, got, want)
		}
	}
}

// hasAMDGPU mirrors the amdgpu package gate: real hardware required.
func hasAMDGPU() bool {
	vendorFiles, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/vendor")
	for _, vendorFile := range vendorFiles {
		b, err := os.ReadFile(vendorFile)
		if err == nil && strings.TrimSpace(string(b)) == "0x1002" {
			return true
		}
	}
	return false
}

func TestRocmModeRegistrationOnHardware(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	splitCount = 10 // these tests pin one slot count for every GPU
	if !hasAMDGPU() {
		t.Skip("no AMD GPU on this machine")
	}
	p := &AMDGPUPlugin{}
	devices := p.getAPIDevices()
	if len(devices) == 0 {
		t.Fatal("rocm mode registered no devices on a machine with an AMD GPU")
	}
	for _, d := range devices {
		if d.ID == "" {
			t.Errorf("device with empty ID: %+v", d)
		}
		if d.Mode != "" {
			t.Errorf("rocm mode device %s has Mode=%q, want empty", d.ID, d.Mode)
		}
		if d.Count != int32(splitCount) {
			t.Errorf("device %s Count=%d, want %d", d.ID, d.Count, splitCount)
		}
		t.Logf("registered: %s (count=%d, CUs=%d, mem=%dMiB, type=%s)", d.ID, d.Count, d.Devcore, d.Devmem, d.Type)
	}
}

type amdsmiGolden struct {
	UUID string `json:"uuid"`
	Type string `json:"type"`
}

func loadAmdsmiGolden(t *testing.T, path string) map[string]amdsmiGolden {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]amdsmiGolden{}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

// flexibleInt tolerates the amd-smi JSON quirk of "" for header fields on
// resource-only rows.
type flexibleInt int

func (f *flexibleInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	*f = flexibleInt(v)
	return err
}

// loadPartitionProfilesGolden parses a raw amd-smi partition -g capture into
// PartitionProfile. Profile rows carry the header fields; the following rows
// only detail other resource types (DECODER/DMA/JPEG) and are skipped.
func loadPartitionProfilesGolden(t *testing.T, path string) []amdsmi.PartitionProfile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		PartitionProfiles []struct {
			ProfileIndex      flexibleInt `json:"profile_index"`
			MemoryCaps        string      `json:"memory_partition_caps"`
			AcceleratorType   string      `json:"accelerator_type"`
			NumPartitions     flexibleInt `json:"num_partitions"`
			ResourceType      string      `json:"resource_type"`
			ResourceInstances flexibleInt `json:"resource_instances"`
		} `json:"partition_profiles"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	profiles := []amdsmi.PartitionProfile{}
	for _, row := range raw.PartitionProfiles {
		if row.AcceleratorType == "" {
			continue
		}
		profiles = append(profiles, amdsmi.PartitionProfile{
			ProfileIndex:    int(row.ProfileIndex),
			Type:            strings.TrimSuffix(row.AcceleratorType, "*"),
			MemoryCaps:      row.MemoryCaps,
			NumPartitions:   int(row.NumPartitions),
			XCCPerPartition: int(row.ResourceInstances),
		})
	}
	return profiles
}

func TestPartitionProfilesFromFixture(t *testing.T) {
	profiles := loadPartitionProfilesGolden(t, "../../../testdata/amdsmi-partition-g3-mi355x.json")
	want := []amdsmi.PartitionProfile{
		{ProfileIndex: 0, Type: "SPX", MemoryCaps: "NPS1", NumPartitions: 1, XCCPerPartition: 8},
		{ProfileIndex: 1, Type: "DPX", MemoryCaps: "NPS1,NPS2", NumPartitions: 2, XCCPerPartition: 4},
		{ProfileIndex: 2, Type: "QPX", MemoryCaps: "NPS1", NumPartitions: 4, XCCPerPartition: 2},
		{ProfileIndex: 3, Type: "CPX", MemoryCaps: "NPS1", NumPartitions: 8, XCCPerPartition: 1},
	}
	if len(profiles) != len(want) {
		t.Fatalf("parsed %d profiles, want %d: %+v", len(profiles), len(want), profiles)
	}
	for i := range want {
		if profiles[i] != want[i] {
			t.Errorf("profile[%d] = %+v, want %+v", i, profiles[i], want[i])
		}
	}
}

// fixtureBDF converts the topology spelling (0000:75:00:0) to the PCI
// spelling (0000:75:00.0) used as the amdsmi fixture key.
func fixtureBDF(bdf string) string {
	if i := strings.LastIndex(bdf, ":"); i >= 0 {
		return bdf[:i] + "." + bdf[i+1:]
	}
	return bdf
}

// loadMemoryPartitionGolden reads the raw amd-smi partition -m capture; the
// file is gpu_id-keyed, so it can only confirm uniformity, not per-BDF values.
func loadMemoryPartitionGolden(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		MemoryPartition []struct {
			GPUID   int    `json:"gpu_id"`
			Current string `json:"current_memory_partition"`
		} `json:"memory_partition"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	parts := make([]string, len(raw.MemoryPartition))
	for _, g := range raw.MemoryPartition {
		parts[g.GPUID] = strings.ToLower(g.Current)
	}
	return parts
}

// TestRegistrationFromFixture drives the full registration flow against the
// captured MI355X sysfs tree (kernel 6.8.0-136) with an injected AMD SMI
// golden, so it runs on machines without GPUs or AMD SMI. On this kernel the
// XCP render minors are absent from KFD topology, so discovery yields only
// the 8 whole-GPU entries. cu mode registers them as soft devices (Count 10,
// unsuffixed); partition mode registers one hard #spx device per GPU
// (Count 1). XCP partition registration on this kernel is impossible by
// construction; it appears on kernels whose KFD topology exposes XCP nodes.
func TestRegistrationFromFixture(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	splitCount = 10 // these tests pin one slot count for every GPU
	stubCapacity(t)
	root := "../../../testdata/sysfs-mi355x-spx/sys"
	golden := loadAmdsmiGolden(t, "../../../testdata/amdsmi-mi355x.json")
	memoryPartitions := loadMemoryPartitionGolden(t, "../../../testdata/amdsmi-partition-m-mi355x.json")
	for i, partition := range memoryPartitions {
		if partition != "nps1" {
			t.Fatalf("fixture GPU %d memory partition %q, want nps1", i, partition)
		}
	}
	profiles := loadPartitionProfilesGolden(t, "../../../testdata/amdsmi-partition-g3-mi355x.json")
	profileBDFs := map[string]bool{}

	newPlugin := func(mode string) *AMDGPUPlugin {
		p := NewAMDGPUPlugin(
			WithSysfsRoot(root),
			WithAmdSMI(
				func(bdfs []string) (map[string]string, error) {
					uuidByBDF := map[string]string{}
					for _, bdf := range bdfs {
						if entry, ok := golden[fixtureBDF(bdf)]; ok {
							uuidByBDF[bdf] = entry.UUID
						}
					}
					return uuidByBDF, nil
				},
				func(bdfs []string) (map[string]string, error) {
					namesByBDF := map[string]string{}
					for _, bdf := range bdfs {
						if entry, ok := golden[fixtureBDF(bdf)]; ok {
							namesByBDF[bdf] = entry.Type
						}
					}
					return namesByBDF, nil
				},
				func(bdfs []string) (map[string]string, error) {
					partitionsByBDF := map[string]string{}
					for _, bdf := range bdfs {
						partitionsByBDF[bdf] = memoryPartitions[0]
					}
					return partitionsByBDF, nil
				},
			),
			WithAMDSPartitionProfiles(func(bdfs []string) (map[string][]amdsmi.PartitionProfile, error) {
				profilesByBDF := map[string][]amdsmi.PartitionProfile{}
				for _, bdf := range bdfs {
					profileBDFs[bdf] = true
					profilesByBDF[bdf] = profiles
				}
				return profilesByBDF, nil
			}),
		)
		p.operatingMode = mode
		return p
	}

	cu := newPlugin("cu")
	cuDevices := cu.getAPIDevices()
	if len(cuDevices) != 8 {
		t.Fatalf("cu mode: registered %d devices, want 8: %+v", len(cuDevices), cuDevices)
	}
	for _, d := range cuDevices {
		if d.Count != int32(splitCount) || d.Mode != "" || strings.Contains(d.ID, "#") {
			t.Fatalf("cu mode device = %+v, want Count=%d, Mode empty, unsuffixed ID", d, splitCount)
		}
	}

	partition := newPlugin("partition")
	partitionDevices := partition.getAPIDevices()
	if len(partitionDevices) != 8 {
		t.Fatalf("partition mode: registered %d devices, want 8: %+v", len(partitionDevices), partitionDevices)
	}
	for _, d := range partitionDevices {
		if d.Count != 1 || d.Mode != "spx" || !strings.HasSuffix(d.ID, "#spx") {
			t.Fatalf("partition mode device = %+v, want Count=1, Mode=spx, #spx ID", d)
		}
	}

	for key, deviceData := range cu.AMDGPUs {
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			t.Error("XCP entries must not reach registration on this kernel")
		}
		if cpt, _ := deviceData["computePartitionType"].(string); cpt != "spx" {
			t.Errorf("%s: computePartitionType %q, want spx", key, cpt)
		}
		if mpt, _ := deviceData["memoryPartitionType"].(string); mpt != "nps1" {
			t.Errorf("%s: memoryPartitionType %q, want nps1 from amd-smi", key, mpt)
		}
	}
	if len(profileBDFs) != 8 {
		t.Errorf("partition-profile lookup consulted %d BDFs, want 8: %v", len(profileBDFs), profileBDFs)
	}
}

func TestDeviceDataFromROCrUUID(t *testing.T) {
	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"amdgpu_xcp_30": {"card": 8, "renderD": 136},
		},
		rocrUUIDToTopology: map[string]string{
			"GPU-466450b96fbde849": "amdgpu_xcp_30",
		},
	}

	for _, uuid := range []string{"GPU-466450b96fbde849", "GPU-466450b96fbde849#cpx"} {
		device, err := p.deviceDataFromAllocationUUID(uuid)
		if err != nil {
			t.Fatalf("resolve ROCr UUID %q: %v", uuid, err)
		}
		if device["card"] != 8 {
			t.Fatalf("resolved device = %#v, want card 8", device)
		}
		rocrUUID, err := p.rocrUUIDFromAllocationUUID(uuid)
		if err != nil {
			t.Fatalf("resolve ROCr UUID %q: %v", uuid, err)
		}
		if rocrUUID != "GPU-466450b96fbde849" {
			t.Fatalf("rocrUUID = %q, want passthrough of the partition ID", rocrUUID)
		}
	}
}
func TestPartitionCapableBDFs(t *testing.T) {
	got := partitionCapableBDFs(map[string]map[string]interface{}{
		"0000:05:00.0":  {"devID": "0000:05:00.0", "computePartitionType": "spx"},
		"0000:06:00.0":  {"devID": "0000:06:00.0", "computePartitionType": ""},
		"amdgpu_xcp_30": {"devID": "0000:05:00.0", "computePartitionType": "spx"},
	})
	if len(got) != 1 || got[0] != "0000:05:00.0" {
		t.Fatalf("partitionCapableBDFs = %v, want only the partitioned whole GPU", got)
	}
}

func TestDevicesForResource(t *testing.T) {
	spx := []*pluginapi.Device{{ID: "a#0"}}
	qpx := []*pluginapi.Device{{ID: "b#qpx"}}
	byType := map[string][]*pluginapi.Device{"spx_nps1": spx, "qpx_nps1": qpx}
	if got := devicesForResource(byType, "qpx_nps1"); len(got) != 1 || got[0].ID != "b#qpx" {
		t.Fatalf("partition resource = %v", got)
	}
	if got := devicesForResource(byType, "gpu"); len(got) != 2 {
		t.Fatalf("single strategy on a mixed node = %v, want every device", got)
	}
}

// ROCr renumbers devices in ROCR_VISIBLE_DEVICES order and HSA_CU_MASK
// indexes the renumbered list, so both must follow the request order.
func TestAllocateMultiGPUMaskOrder(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{
			utils.BindTimeAnnotations:     "1",
			utils.AssignedNodeAnnotations: "n",
			utils.DeviceBindPhase:         utils.DeviceBindAllocating,
			utils.DeviceToAllocate:        "uuid-b,AMDGPU,512,2:uuid-a,AMDGPU,16304,64:;",
		}},
		Spec:   corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}, pod)
	old := utils.KubeClient
	t.Cleanup(func() { utils.KubeClient = old })
	utils.KubeClient = cs
	t.Setenv(utils.NodeNameEnvName, "n")

	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"0000:08:00.0": {"card": 1, "renderD": 128},
			"0000:0a:00.0": {"card": 2, "renderD": 130},
		},
		amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:08:00.0", "uuid-b": "0000:0a:00.0"},
		amdSMIUUIDToROCrUUID: map[string]string{"uuid-a": "GPU-a", "uuid-b": "GPU-b"},
		deviceCache: []*utils.DeviceInfo{
			{ID: "uuid-a", Devcore: 64, Devmem: 16304},
			{ID: "uuid-b", Devcore: 2, Devmem: 512},
		},
	}
	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"0000:0a:00.0#0", "0000:08:00.0#0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envs := resp.ContainerResponses[0].Envs
	for k, want := range map[string]string{
		"ROCR_VISIBLE_DEVICES": "GPU-b,GPU-a",
		"HSA_CU_MASK":          "0:0-1;1:0-63",
		"HIP_VISIBLE_DEVICES":  "0,1",
	} {
		if envs[k] != want {
			t.Errorf("%s = %q, want %q", k, envs[k], want)
		}
	}
}

// An APU reports KFD unique_id 0, so ROCr has no UUID for it and only the
// index among the container's GPUs, in render node order, names it.
func TestRocrVisibleListIndexFallback(t *testing.T) {
	for _, tc := range []struct {
		uuids  []string
		minors []int
		want   string
	}{
		{[]string{"GPU-a", "GPU-b"}, []int{128, 130}, "GPU-a,GPU-b"},
		{[]string{"", "GPU-a"}, []int{130, 128}, "1,GPU-a"},
		{[]string{"GPU-a", ""}, []int{128, 130}, "GPU-a,1"},
		{[]string{""}, []int{130}, "0"},
	} {
		if got := rocrVisibleList(tc.uuids, tc.minors); got != tc.want {
			t.Errorf("rocrVisibleList(%v, %v) = %q, want %q", tc.uuids, tc.minors, got, tc.want)
		}
	}
	p := &AMDGPUPlugin{amdSMIUUIDToROCrUUID: map[string]string{"apu": ""}}
	if got, err := p.rocrUUIDFromAllocationUUID("apu"); err != nil || got != "" {
		t.Errorf("known GPU without ROCr UUID = %q, %v; want empty, nil", got, err)
	}
	if _, err := p.rocrUUIDFromAllocationUUID("missing"); err == nil {
		t.Error("unknown GPU must still fail")
	}
}

func TestCUMaskUnit(t *testing.T) {
	dir := t.TempDir()
	for node, gfx := range map[string]string{"1": "120001", "2": "100306", "3": "90402"} {
		if err := os.MkdirAll(filepath.Join(dir, node), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, node, "properties"), []byte("gfx_target_version "+gfx+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"rdna4": {"nodeId": 1}, "rdna2-apu": {"nodeId": 2}, "cdna3": {"nodeId": 3},
		},
		amdSMIUUIDToTopology: map[string]string{"a": "rdna4", "b": "rdna2-apu", "c": "cdna3"},
	}
	for uuid, want := range map[string]int{"a": 2, "b": 2, "c": 1, "unknown": 1} {
		if got := p.cuMaskUnit(uuid, dir); got != want {
			t.Errorf("cuMaskUnit(%s) = %d, want %d", uuid, got, want)
		}
	}
}

// libamvgpu caps each container-local device by HIP_DEVICE_MEMORY_LIMIT_<i>;
// one shared limit capped every GPU of a multi-GPU slice at the first's size.
func TestAllocatePerGPUMemoryLimit(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{
			utils.BindTimeAnnotations:     "1",
			utils.AssignedNodeAnnotations: "n",
			utils.DeviceBindPhase:         utils.DeviceBindAllocating,
			utils.DeviceToAllocate:        "uuid-b,AMDGPU,4096,1:uuid-a,AMDGPU,16304,32:;",
		}},
		Spec:   corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Annotations: map[string]string{
		utils.NodeLockKey: time.Now().Format(time.RFC3339) + ",ns,p",
	}}}
	cs := fake.NewSimpleClientset(node, pod)
	old := utils.KubeClient
	t.Cleanup(func() { utils.KubeClient = old })
	utils.KubeClient = cs
	t.Setenv(utils.NodeNameEnvName, "n")

	p := &AMDGPUPlugin{
		AMDGPUs: map[string]map[string]interface{}{
			"0000:08:00.0": {"card": 1, "renderD": 128},
			"0000:0a:00.0": {"card": 2, "renderD": 130},
		},
		amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:08:00.0", "uuid-b": "0000:0a:00.0"},
		amdSMIUUIDToROCrUUID: map[string]string{"uuid-a": "GPU-a", "uuid-b": "GPU-b"},
		deviceCache: []*utils.DeviceInfo{
			{ID: "uuid-a", Devcore: 64, Devmem: 16304},
			{ID: "uuid-b", Devcore: 2, Devmem: 4096},
		},
	}
	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"0000:0a:00.0#0", "0000:08:00.0#0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	envs := resp.ContainerResponses[0].Envs
	for k, want := range map[string]string{"HIP_DEVICE_MEMORY_LIMIT_0": "4096m", "HIP_DEVICE_MEMORY_LIMIT_1": "16304m"} {
		if envs[k] != want {
			t.Errorf("%s = %q, want %q", k, envs[k], want)
		}
	}
	if m := resp.ContainerResponses[0].Mounts; len(m) != 1 || m[0].ContainerPath != "/usr/local/vgpu/libamvgpu.so" {
		t.Errorf("sliced allocation mounts %v, want the hook", m)
	}
}

// The scheduler rounds core requests by the published cuPerWGP, so it must
// match what Allocate uses.
func TestDeviceCustomInfo(t *testing.T) {
	dir := t.TempDir()
	for node, props := range map[string]string{"1": "gfx_target_version 120001\nnum_cp_queues 4\n", "2": "gfx_target_version 90402\n"} {
		if err := os.MkdirAll(filepath.Join(dir, node), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, node, "properties"), []byte(props), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rdna := deviceCustomInfo("0000:08:00.0", dir, 1)
	if rdna["cuPerWGP"] != 2 || rdna["computeQueues"] != int64(4) || rdna["pciBDF"] != "0000:08:00.0" {
		t.Errorf("RDNA custominfo = %v", rdna)
	}
	if cdna := deviceCustomInfo("0000:0A:00.0", dir, 2); cdna["cuPerWGP"] != 1 || cdna["pciBDF"] != "0000:0a:00.0" {
		t.Errorf("CDNA custominfo = %v", cdna)
	}
}

// Each GPU of a multi-GPU slice gets its own dmem region capped at its own
// share, not the first GPU's.
func TestApplyDmemCapPerGPU(t *testing.T) {
	root := t.TempDir()
	pod := dmem.PodCgroupPath(root, "u-1", corev1.PodQOSBestEffort)
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"cgroup.controllers": "cpu memory dmem\n",
		"dmem.capacity":      "drm/0000:08:00.0/vram 17095983104\ndrm/0000:0a:00.0/vram 4294967296\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &AMDGPUPlugin{
		dmemCgroupRoot: root,
		AMDGPUs: map[string]map[string]interface{}{
			"0000:08:00.0": {"devID": "0000:08:00:0"},
			"0000:0a:00.0": {"devID": "0000:0a:00:0"},
		},
		amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:08:00.0", "uuid-b": "0000:0a:00.0"},
	}
	for _, tc := range []struct {
		d    utils.ContainerDevice
		want string
	}{
		{utils.ContainerDevice{UUID: "uuid-b", Usedmem: 1024}, "drm/0000:0a:00.0/vram 1073741824\n"},
		{utils.ContainerDevice{UUID: "uuid-a", Usedmem: 8192}, "drm/0000:08:00.0/vram 8589934592\n"},
	} {
		p.applyDmemCap("ns", "p", "u-1", corev1.PodQOSBestEffort, tc.d)
		// cgroupfs applies each write to its region; a plain file keeps the last.
		if got, _ := os.ReadFile(filepath.Join(pod, "dmem.max")); string(got) != tc.want {
			t.Errorf("dmem.max after %s = %q, want %q", tc.d.UUID, got, tc.want)
		}
	}
}

// A memory-only slice shares every CU, so it must not hold them: a second
// memory-only pod on the same GPU would otherwise find no free CUs.
func TestAllocateMemoryOnlySliceHoldsNoCUs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{
			utils.BindTimeAnnotations:     "1",
			utils.AssignedNodeAnnotations: "n",
			utils.DeviceBindPhase:         utils.DeviceBindAllocating,
			utils.DeviceToAllocate:        "uuid-a,AMDGPU,4096,0:;",
		}},
		Spec:   corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Annotations: map[string]string{
		utils.NodeLockKey: time.Now().Format(time.RFC3339) + ",ns,p",
	}}}
	cs := fake.NewSimpleClientset(node, pod)
	old := utils.KubeClient
	t.Cleanup(func() { utils.KubeClient = old })
	utils.KubeClient = cs
	t.Setenv(utils.NodeNameEnvName, "n")

	p := &AMDGPUPlugin{
		AMDGPUs:              map[string]map[string]interface{}{"0000:08:00.0": {"card": 1, "renderD": 128}},
		amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:08:00.0"},
		amdSMIUUIDToROCrUUID: map[string]string{"uuid-a": "GPU-a"},
		deviceCache:          []*utils.DeviceInfo{{ID: "uuid-a", Devcore: 64, Devmem: 16304}},
	}
	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"0000:08:00.0#0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ContainerResponses[0].Envs["HSA_CU_MASK"]; got != "0:0-63" {
		t.Errorf("HSA_CU_MASK = %q, want every CU", got)
	}
	if got := resp.ContainerResponses[0].Envs["HIP_DEVICE_MEMORY_LIMIT_0"]; got != "4096m" {
		t.Errorf("HIP_DEVICE_MEMORY_LIMIT_0 = %q, want 4096m", got)
	}
	got, err := cs.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cu, held := got.Annotations[utils.CuAllocation]; held {
		t.Errorf("memory-only slice holds CUs %s", cu)
	}
}

// Discovery swaps the device maps every 30s while Allocate, ListAndWatch and
// the dmem goroutines read them; run with -race.
func TestPublishWhileReading(t *testing.T) {
	p := &AMDGPUPlugin{}
	gpus := map[string]map[string]interface{}{"0000:08:00.0": {"card": 1, "renderD": 128}}
	ids := map[string]string{"uuid-a": "0000:08:00.0"}
	cache := []*utils.DeviceInfo{{ID: "uuid-a", Devcore: 64}}
	p.publish(gpus, ids, map[string]string{"uuid-a": "GPU-a"}, map[string]string{"0000:08:00.0": "GPU-a"}, nil, nil, cache)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			p.publish(gpus, ids, map[string]string{"uuid-a": "GPU-a"}, map[string]string{"0000:08:00.0": "GPU-a"}, nil, nil, cache)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = p.deviceDataFromAllocationUUID("uuid-a")
			_, _ = p.rocrUUIDFromAllocationUUID("0000:08:00.0#1")
			_ = p.lookupDevice("uuid-a")
			p.markNonFunctional([]*pluginapi.Device{{ID: "0000:08:00.0#0"}}, func(string) bool { return true })
		}
	}()
	wg.Wait()
}

func TestKubeletDevices(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	splitCount = 3
	gpus := map[string]map[string]interface{}{
		"a": {"numaNode": 0, "nodeId": 1, "computePartitionType": "spx", "memoryPartitionType": "nps1"},
		"b": {"numaNode": 1, "nodeId": 2, "computePartitionType": "cpx", "memoryPartitionType": "nps4"},
	}
	if got := len((&AMDGPUPlugin{operatingMode: "cu"}).kubeletDevices(gpus, true, "gpu")); got != 6 {
		t.Errorf("homogeneous node lists %d devices, want 6", got)
	}
	devs := (&AMDGPUPlugin{operatingMode: "cu"}).kubeletDevices(gpus, false, "cpx_nps4")
	if len(devs) != 3 || !strings.HasPrefix(devs[0].ID, "b#") || devs[0].Topology.Nodes[0].ID != 1 {
		t.Errorf("cpx_nps4 resource lists %v, want the 3 slots of b on NUMA 1", devs)
	}
}

// fakeListAndWatch fails every send after the first okSends.
type fakeListAndWatch struct {
	grpc.ServerStream
	ctx     context.Context
	okSends int
	sends   int
	sent    []*pluginapi.ListAndWatchResponse
}

func (f *fakeListAndWatch) Send(r *pluginapi.ListAndWatchResponse) error {
	f.sent = append(f.sent, r)
	f.sends++
	if f.sends > f.okSends {
		return errors.New("transport closed")
	}
	return nil
}
func (f *fakeListAndWatch) Context() context.Context { return f.ctx }

// A stream must end when kubelet goes away or a send fails; a stale one
// would keep taking the heartbeats meant for kubelet's new stream.
func TestServeDevicesEndsWithTheStream(t *testing.T) {
	devs := []*pluginapi.Device{{ID: "a#0"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &AMDGPUPlugin{Heartbeat: make(chan bool)}
	if err := p.serveDevices(&fakeListAndWatch{ctx: ctx, okSends: 1}, devs, func(string) bool { return true }); err != nil {
		t.Errorf("closed stream: err = %v, want nil", err)
	}

	beat := make(chan bool, 1)
	beat <- true
	p = &AMDGPUPlugin{Heartbeat: beat}
	done := make(chan error, 1)
	go func() {
		done <- p.serveDevices(&fakeListAndWatch{ctx: context.Background(), okSends: 1}, devs, func(string) bool { return true })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("failed send: err = nil, want it returned")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveDevices kept running after a failed send")
	}
}

// A GPU whose capacity cannot be read has no CU or VRAM count; registering
// it would fail every slice on it and the CU rebuild for the whole node.
func TestGetAPIDevicesSkipsGPUWithoutCapacity(t *testing.T) {
	if _, err := os.Stat("/sys/module/amdgpu/drivers/"); err != nil {
		t.Skip("no amdgpu driver")
	}
	defer func(f func(string) (amdgpu.DeviceCapacity, error)) { getDeviceCapacity = f }(getDeviceCapacity)
	getDeviceCapacity = func(string) (amdgpu.DeviceCapacity, error) {
		return amdgpu.DeviceCapacity{}, errors.New("ioctl failed")
	}
	p := &AMDGPUPlugin{}
	if devs := p.getAPIDevices(); len(devs) != 0 {
		t.Errorf("registered %d GPUs without capacity, want none", len(devs))
	}
}
