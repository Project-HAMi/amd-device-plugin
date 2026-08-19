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

package amdgpu

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func hasAMDGPU() bool {
	vendorFiles, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/vendor")
	for _, vendorFile := range vendorFiles {
		vendor, err := ioutil.ReadFile(vendorFile)
		if err == nil && strings.TrimSpace(string(vendor)) == "0x1002" {
			return true
		}
	}
	return false
}

func TestFirmwareVersionConsistent(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("Skipping test, no AMD GPU found.")
	}

	devices := GetAMDGPUs()

	for pci, dev := range devices {
		card := fmt.Sprintf("card%d", dev["card"])
		t.Logf("%s, %s", pci, card)

		//debugfs path/interface may not be stable
		debugFSfeatVersion, debugFSfwVersion :=
			parseDebugFSFirmwareInfo("/sys/kernel/debug/dri/" + card[4:] + "/amdgpu_firmware_info")
		featVersion, fwVersion, err := GetFirmwareVersions(card)
		if err != nil {
			// Device exists but the DRM node is not usable (e.g. accel not
			// working on handheld/APU kernels); nothing to compare.
			t.Skipf("Skipping, DRM device %s not usable: %s", card, err.Error())
		}

		for k := range featVersion {
			if featVersion[k] != debugFSfeatVersion[k] {
				t.Errorf("%s feature version not consistent: ioctl: %d, debugfs: %d",
					k, featVersion[k], debugFSfeatVersion[k])
			}
			if fwVersion[k] != debugFSfwVersion[k] {
				t.Errorf("%s firmware version not consistent: ioctl: %x, debugfs: %x",
					k, fwVersion[k], debugFSfwVersion[k])
			}
		}
	}
}

func TestAMDGPUcountConsistent(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("Skipping test, no AMD GPU found.")
	}

	devices := GetAMDGPUs()

	matches, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/vendor")

	count := 0
	for _, vidPath := range matches {
		t.Log(vidPath)
		b, err := ioutil.ReadFile(vidPath)
		vid := string(b)

		// AMD vendor ID is 0x1002
		if err == nil && "0x1002" == strings.TrimSpace(vid) {
			count++
		} else {
			t.Log(vid)
		}

	}

	if count != len(devices) {
		t.Errorf("AMD GPU counts differ: /sys/module/amdgpu: %d, /sys/class/drm: %d", len(devices), count)
	}

}

func TestHasAMDGPU(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("Skipping test, no AMD GPU found.")
	}
}

func TestDevFunctional(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("Skipping test, no AMD GPU found.")
	}

	devices := GetAMDGPUs()

	for _, dev := range devices {
		card := fmt.Sprintf("card%d", dev["card"])

		ret := DevFunctional(card)
		t.Logf("%s functional: %t", card, ret)
	}
}

func TestParseTopologyProperties(t *testing.T) {
	var v int64
	var e error
	var re *regexp.Regexp
	var path string

	re = regexp.MustCompile(`size_in_bytes\s(\d+)`)
	path = "../../../testdata/topology-parsing/topology/nodes/1/mem_banks/0/properties"
	v, _ = ParseTopologyProperties(path, re)
	if v != 17163091968 {
		t.Errorf("Error parsing %s for `%s`: expect %d", path, re.String(), 17163091968)
	}

	re = regexp.MustCompile(`flags\s(\d+)`)
	path = "../../../testdata/topology-parsing/topology/nodes/1/mem_banks/0/properties"
	v, _ = ParseTopologyProperties(path, re)
	if v != 0 {
		t.Errorf("Error parsing %s for `%s`: expect %d", path, re.String(), 0)
	}

	re = regexp.MustCompile(`simd_count\s(\d+)`)
	path = "../../../testdata/topology-parsing/topology/nodes/2/properties"
	v, _ = ParseTopologyProperties(path, re)
	if v != 256 {
		t.Errorf("Error parsing %s for `%s`: expect %d", path, re.String(), 256)
	}

	re = regexp.MustCompile(`simd_id_base\s(\d+)`)
	path = "../../../testdata/topology-parsing/topology/nodes/2/properties"
	v, _ = ParseTopologyProperties(path, re)
	if v != 2147487744 {
		t.Errorf("Error parsing %s for `%s`: expect %d", path, re.String(), 2147487744)
	}

	re = regexp.MustCompile(`asdf\s(\d+)`)
	path = "../../../testdata/topology-parsing/topology/nodes/2/properties"
	_, e = ParseTopologyProperties(path, re)
	if e == nil {
		t.Errorf("Error parsing %s for `%s`: expect error", path, re.String())
	}

}

func TestParseDebugFSFirmwareInfo(t *testing.T) {
	expFeat := map[string]uint32{
		"VCE":   0,
		"UVD":   0,
		"MC":    0,
		"ME":    35,
		"PFP":   35,
		"CE":    35,
		"RLC":   0,
		"MEC":   33,
		"MEC2":  33,
		"SOS":   0,
		"ASD":   0,
		"SMC":   0,
		"SDMA0": 40,
		"SDMA1": 40,
	}

	expFw := map[string]uint32{
		"VCE":   0x352d0400,
		"UVD":   0x01571100,
		"MC":    0x00000000,
		"ME":    0x00000094,
		"PFP":   0x000000a4,
		"CE":    0x0000004a,
		"RLC":   0x00000058,
		"MEC":   0x00000160,
		"MEC2":  0x00000160,
		"SOS":   0x00161a92,
		"ASD":   0x0016129a,
		"SMC":   0x001c2800,
		"SDMA0": 0x00000197,
		"SDMA1": 0x00000197,
	}

	feat, fw := parseDebugFSFirmwareInfo("../../../testdata/debugfs-parsing/amdgpu_firmware_info")

	for k := range expFeat {
		val, ok := feat[k]
		if !ok || val != expFeat[k] {
			t.Errorf("Error parsing feature version for %s: expect %d", k, expFeat[k])
		}
	}

	for k := range expFw {
		val, ok := fw[k]
		if !ok || val != expFw[k] {
			t.Errorf("Error parsing firmware version for %s: expect %#08x", k, expFw[k])
		}
	}
	if len(feat) != len(expFeat) || len(fw) != len(expFw) {
		t.Errorf("Incorrect parsing of amdgpu firmware info from debugfs")
	}
}

func TestRenderDevIdsFromTopology(t *testing.T) {
	renderDevIds := GetDevIdsFromTopology("../../../testdata/topology-parsing-mi308")

	expDevIds := map[int]string{
		128: "0000:0a:00:0",
		129: "0000:0a:00:0",
		130: "0000:0a:00:0",
		131: "0000:0a:00:0",
		136: "0000:80:00:0",
		137: "0000:80:00:0",
		138: "0000:80:00:0",
		139: "0000:80:00:0",
		144: "0000:a4:00:0",
		145: "0000:a4:00:0",
		146: "0000:a4:00:0",
		147: "0000:a4:00:0",
		152: "0000:c8:00:0",
		153: "0000:c8:00:0",
		154: "0000:c8:00:0",
		155: "0000:c8:00:0",
		160: "0001:0b:00:0",
		161: "0001:0b:00:0",
		162: "0001:0b:00:0",
		163: "0001:0b:00:0",
		168: "0001:81:00:0",
		169: "0001:81:00:0",
		170: "0001:81:00:0",
		171: "0001:81:00:0",
		176: "0001:a5:00:0",
		177: "0001:a5:00:0",
		178: "0001:a5:00:0",
		179: "0001:a5:00:0",
		184: "0001:c9:00:0",
		185: "0001:c9:00:0",
		186: "0001:c9:00:0",
		187: "0001:c9:00:0"}
	if !reflect.DeepEqual(renderDevIds, expDevIds) {
		val, _ := json.MarshalIndent(renderDevIds, "", "  ")
		exp, _ := json.MarshalIndent(expDevIds, "", "  ")

		t.Errorf("RenderNode set was incorrect")
		t.Errorf("Got: %s", val)
		t.Errorf("Want: %s", exp)
	}
}

func TestROCrUUIDsFromTopology(t *testing.T) {
	got := GetROCrUUIDsFromTopology("../../../testdata/topology-parsing")
	want := map[int]string{
		128: "GPU-c34ec50444dd0a6c",
		129: "GPU-c34ec50444dd0a6d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ROCr UUIDs = %#v, want %#v", got, want)
	}
}

func writeTopologyProperties(t *testing.T, root string, node int, renderMinor, uniqueID uint64) {
	t.Helper()
	dir := filepath.Join(root, "topology", "nodes", fmt.Sprintf("%d", node))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("simd_count 24\ndrm_render_minor %d\nunique_id %d\n", renderMinor, uniqueID)
	if err := os.WriteFile(filepath.Join(dir, "properties"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mi355xSysfs is the captured sysfs tree of an 8x MI355X node in SPX/NPS1 on
// kernel 6.8.0-136 (testdata/sysfs-mi355x-spx/README.md).
const mi355xSysfs = "../../../testdata/sysfs-mi355x-spx/sys"
const mi355xKFD = mi355xSysfs + "/class/kfd/kfd"

func TestGetAMDGPUsFromFixture(t *testing.T) {
	devices := GetAMDGPUs(mi355xSysfs)

	want := map[string]struct {
		card, renderD, nodeId, numaNode int
		devID                           string
	}{
		"0000:75:00.0": {card: 1, renderD: 128, nodeId: 8, numaNode: 0, devID: "0000:75:00:0"},
		"0000:05:00.0": {card: 9, renderD: 136, nodeId: 9, numaNode: 1, devID: "0000:05:00:0"},
		"0000:65:00.0": {card: 17, renderD: 144, nodeId: 10, numaNode: 3, devID: "0000:65:00:0"},
		"0000:15:00.0": {card: 25, renderD: 152, nodeId: 11, numaNode: 2, devID: "0000:15:00:0"},
		"0000:f5:00.0": {card: 33, renderD: 160, nodeId: 12, numaNode: 4, devID: "0000:f5:00:0"},
		"0000:85:00.0": {card: 41, renderD: 168, nodeId: 13, numaNode: 5, devID: "0000:85:00:0"},
		"0000:e5:00.0": {card: 49, renderD: 176, nodeId: 14, numaNode: 7, devID: "0000:e5:00:0"},
		"0000:95:00.0": {card: 57, renderD: 184, nodeId: 15, numaNode: 6, devID: "0000:95:00:0"},
	}
	if len(devices) != len(want) {
		t.Fatalf("GetAMDGPUs returned %d devices, want %d: %v", len(devices), len(want), devices)
	}
	for bdf, exp := range want {
		dev, ok := devices[bdf]
		if !ok {
			t.Errorf("missing device %s", bdf)
			continue
		}
		if dev["card"] != exp.card || dev["renderD"] != exp.renderD || dev["nodeId"] != exp.nodeId || dev["numaNode"] != exp.numaNode {
			t.Errorf("%s: got card=%v renderD=%v nodeId=%v numaNode=%v, want card=%d renderD=%d nodeId=%d numaNode=%d",
				bdf, dev["card"], dev["renderD"], dev["nodeId"], dev["numaNode"], exp.card, exp.renderD, exp.nodeId, exp.numaNode)
		}
		if dev["devID"] != exp.devID {
			t.Errorf("%s: devID=%v, want %s", bdf, dev["devID"], exp.devID)
		}
		if dev["computePartitionType"] != "spx" || dev["memoryPartitionType"] != "nps1" {
			t.Errorf("%s: partition=%v/%v, want spx/nps1", bdf, dev["computePartitionType"], dev["memoryPartitionType"])
		}
	}
	for key := range devices {
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			t.Errorf("unexpected XCP entry %s: this kernel does not publish XCP render minors in KFD topology", key)
		}
	}
}

func TestGetDevIdsFromTopologyMI355X(t *testing.T) {
	got := GetDevIdsFromTopology(mi355xKFD)
	want := map[int]string{
		128: "0000:75:00:0", 136: "0000:05:00:0", 144: "0000:65:00:0", 152: "0000:15:00:0",
		160: "0000:f5:00:0", 168: "0000:85:00:0", 176: "0000:e5:00:0", 184: "0000:95:00:0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("renderDevIds = %#v, want %#v", got, want)
	}
}

func TestGetROCrUUIDsFromTopologyMI355X(t *testing.T) {
	got := GetROCrUUIDsFromTopology(mi355xKFD)
	want := map[int]string{
		128: "GPU-08da82c12ef81b5d",
		136: "GPU-c62ac130ee448403",
		144: "GPU-8aa2810481bf6315",
		152: "GPU-f32cb37ceef6feb5",
		160: "GPU-e88fa10fb8782ebc",
		168: "GPU-cf10e0d8795d487e",
		176: "GPU-6402daab8ae76e4e",
		184: "GPU-1303b4d1d1d0c537",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ROCr UUIDs = %#v, want %#v", got, want)
	}
}

func TestGetROCrIndexesFromTopology(t *testing.T) {
	root := t.TempDir()
	// node 0 is the CPU (no renderD), node 1 is an APU with unique_id 0,
	// node 2 is a discrete GPU with a real unique_id.
	writeTopologyProperties(t, root, 0, 0, 0)
	writeTopologyProperties(t, root, 1, 128, 0)
	writeTopologyProperties(t, root, 2, 129, 5)

	got := GetROCrIndexesFromTopology(root)
	if idx, ok := got[128]; !ok || idx != 0 {
		t.Errorf("index for renderD128 = %d, %v; want 0, true", idx, ok)
	}
	if _, ok := got[129]; ok {
		t.Error("renderD129 has a real unique_id and must not get an index fallback")
	}
}

func TestPartitionCapacity(t *testing.T) {
	// MI355X whole-GPU values: 294896 MB VRAM (amdsmi-static fixture) and
	// 304 CUs (8 XCCs x 38).
	whole := DeviceCapacity{VRAMMiB: 287984, CUCount: 304}

	tests := []struct {
		name        string
		partitions  int
		wantVRAMMiB int32
		wantCUCount int32
	}{
		{"SPX", 1, 287984, 304},
		{"DPX", 2, 143992, 152},
		{"QPX", 4, 71996, 76},
		{"CPX", 8, 35998, 38},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PartitionCapacity(whole, tt.partitions)
			if got.VRAMMiB != tt.wantVRAMMiB || got.CUCount != tt.wantCUCount {
				t.Errorf("PartitionCapacity(%d) = %d MiB, %d CUs; want %d MiB, %d CUs",
					tt.partitions, got.VRAMMiB, got.CUCount, tt.wantVRAMMiB, tt.wantCUCount)
			}
		})
	}
}
