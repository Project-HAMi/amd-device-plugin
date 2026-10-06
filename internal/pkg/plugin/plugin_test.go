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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestCountGPUDevFromTopology(t *testing.T) {
	count := countGPUDevFromTopology("../../../testdata/topology-parsing")

	expCount := 2
	if count != expCount {
		t.Errorf("Count was incorrect, got: %d, want: %d.", count, expCount)
	}
}

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

	device, err := p.deviceDataFromAllocationUUID("8eff74b5-0000-1000-801b-b56457addd1b", "node-a")
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

func TestListerSplitCount(t *testing.T) {
	defer func(n int) { splitCount = n }(splitCount)
	(&AMDGPULister{}).NewPlugin("gpu")
	if splitCount != 10 {
		t.Fatalf("default splitCount = %d, want 10", splitCount)
	}
	(&AMDGPULister{SplitCount: 2}).NewPlugin("gpu")
	if splitCount != 2 {
		t.Errorf("splitCount = %d, want 2", splitCount)
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
		utils.KubeClient = cs
		t.Setenv(utils.NodeNameEnvName, "n")

		p := &AMDGPUPlugin{
			AMDGPUs:              map[string]map[string]interface{}{"0000:06:00.0": {"card": 1, "renderD": 129}},
			amdSMIUUIDToTopology: map[string]string{"uuid-a": "0000:06:00.0"},
			amdSMIUUIDToROCrUUID: map[string]string{"uuid-a": "GPU-1"},
			deviceCache:          []*utils.DeviceInfo{{ID: "uuid-a", Devcore: 32, Devmem: 16304}},
		}
		_, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
			ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"0000:06:00.0#0"}}},
		})
		utils.KubeClient = old
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: Allocate error = %v, want error %v", tc.name, err, tc.wantErr)
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
}
