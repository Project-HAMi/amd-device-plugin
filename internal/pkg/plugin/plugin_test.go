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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
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
	p.publish(gpus, ids, map[string]string{"uuid-a": "GPU-a"}, map[string]string{"0000:08:00.0": "GPU-a"}, cache)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			p.publish(gpus, ids, map[string]string{"uuid-a": "GPU-a"}, map[string]string{"0000:08:00.0": "GPU-a"}, cache)
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
	if got := len(kubeletDevices(gpus, true, "gpu")); got != 6 {
		t.Errorf("homogeneous node lists %d devices, want 6", got)
	}
	devs := kubeletDevices(gpus, false, "cpx_nps4")
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
}

func (f *fakeListAndWatch) Send(*pluginapi.ListAndWatchResponse) error {
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
