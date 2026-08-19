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
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	_, delta, err := cuallocation.AllocateN(occupied[uuid], totalCUs, 4)
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

func TestIsSchedulableTopologyKey(t *testing.T) {
	wholeGPU := map[string]interface{}{"devID": "0000:05:00.0", "computePartitionType": "spx", "memoryPartitionType": "nps1"}
	qpxParent := map[string]interface{}{"devID": "0000:15:00.0", "computePartitionType": "qpx", "memoryPartitionType": "nps1"}
	noPartition := map[string]interface{}{"devID": "0000:75:00.0", "computePartitionType": "", "memoryPartitionType": ""}
	xcp := map[string]interface{}{"devID": "0000:15:00.0", "computePartitionType": "qpx", "memoryPartitionType": "nps1"}
	// partitionsByBDF: spx=1, qpx=4, none=0 (unknown -> spx default).
	partitions := map[string]int{"0000:05:00.0": 1, "0000:15:00.0": 4}

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

func TestHardEntryRegistration(t *testing.T) {
	xcp := map[string]interface{}{"computePartitionType": "cpx", "memoryPartitionType": "nps1"}
	noPartition := map[string]interface{}{"computePartitionType": "", "memoryPartitionType": ""}
	wholeGPU := map[string]interface{}{"computePartitionType": "spx", "memoryPartitionType": "nps1"}

	soft := &utils.DeviceInfo{ID: "GPU-466450b96fbde849", Count: defaultSplitCount, Mode: ""}
	if !(&AMDGPUPlugin{operatingMode: "cu"}).registerHardEntry("amdgpu_xcp_30", xcp, soft) {
		t.Fatal("XCP partition should register a hard entry")
	}
	if soft.ID != "GPU-466450b96fbde849#cpx" || soft.Mode != "cpx" || soft.Count != 1 {
		t.Fatalf("hard entry = %+v, want suffixed ID, Mode=cpx, Count=1", soft)
	}

	soft = &utils.DeviceInfo{ID: "uuid-1", Count: defaultSplitCount}
	if (&AMDGPUPlugin{operatingMode: "cu"}).registerHardEntry("0000:05:00.0", wholeGPU, soft) {
		t.Fatal("cu mode: GPU parents must not get a hard entry")
	}
	soft = &utils.DeviceInfo{ID: "uuid-2", Count: defaultSplitCount}
	if (&AMDGPUPlugin{operatingMode: "partition"}).registerHardEntry("0000:05:00.0", wholeGPU, soft) {
		if soft.ID != "uuid-2#spx" || soft.Mode != "spx" || soft.Count != 1 {
			t.Fatalf("partition-mode hard entry = %+v, want ID uuid-2#spx, Mode=spx, Count=1", soft)
		}
	} else {
		t.Fatal("partition mode: SPX whole GPU should get a hard entry")
	}
	soft = &utils.DeviceInfo{ID: "uuid-3", Count: defaultSplitCount}
	if (&AMDGPUPlugin{operatingMode: "partition"}).registerHardEntry("0000:75:00.0", noPartition, soft) {
		t.Fatal("GPU without a compute partition type should not get a hard entry")
	}
}

func TestStripPartitionModeSuffix(t *testing.T) {
	for id, want := range map[string]string{
		"GPU-466450b96fbde849#cpx":             "GPU-466450b96fbde849",
		"GPU-466450b96fbde849#spx":             "GPU-466450b96fbde849",
		"GPU-466450b96fbde849":                 "GPU-466450b96fbde849",
		"GPU-466450b96fbde849#0":               "GPU-466450b96fbde849#0",
		"8eff74b5-0000-1000-801b-b56457addd1b": "8eff74b5-0000-1000-801b-b56457addd1b",
	} {
		if got := stripPartitionModeSuffix(id); got != want {
			t.Errorf("stripPartitionModeSuffix(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestHardKubeletDeviceID(t *testing.T) {
	xcp := map[string]interface{}{"computePartitionType": "cpx", "memoryPartitionType": "nps1"}
	if got := hardKubeletDeviceID("amdgpu_xcp_30", xcp); got != "amdgpu_xcp_30#cpx" {
		t.Errorf("hardKubeletDeviceID(xcp) = %q", got)
	}
	if got := hardKubeletDeviceID("0000:75:00.0", map[string]interface{}{}); got != "" {
		t.Errorf("hardKubeletDeviceID(whole) = %q, want empty", got)
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
		if d.Count != defaultSplitCount {
			t.Errorf("device %s Count=%d, want %d", d.ID, d.Count, defaultSplitCount)
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
func loadPartitionProfilesGolden(t *testing.T, path string) []amdgpu.PartitionProfile {
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
	profiles := []amdgpu.PartitionProfile{}
	for _, row := range raw.PartitionProfiles {
		if row.AcceleratorType == "" {
			continue
		}
		profiles = append(profiles, amdgpu.PartitionProfile{
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
	want := []amdgpu.PartitionProfile{
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
			WithAMDSPartitionProfiles(func(bdfs []string) (map[string][]amdgpu.PartitionProfile, error) {
				profilesByBDF := map[string][]amdgpu.PartitionProfile{}
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
		if d.Count != defaultSplitCount || d.Mode != "" || strings.Contains(d.ID, "#") {
			t.Fatalf("cu mode device = %+v, want Count=%d, Mode empty, unsuffixed ID", d, defaultSplitCount)
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
		device, err := p.deviceDataFromAllocationUUID(uuid, "node-a")
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
