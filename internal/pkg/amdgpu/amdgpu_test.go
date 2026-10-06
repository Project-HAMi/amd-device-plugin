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
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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
		vendor, err := os.ReadFile(vendorFile)
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

		// debugfs path/interface may not be stable
		debugFSfeatVersion, debugFSfwVersion :=
			parseDebugFSFirmwareInfo("/sys/kernel/debug/dri/" + card[4:] + "/amdgpu_firmware_info")
		if len(debugFSfeatVersion) == 0 && len(debugFSfwVersion) == 0 {
			t.Skipf("debugfs amdgpu_firmware_info unavailable for %s; skipping ioctl/debugfs consistency check", card)
		}
		featVersion, fwVersion, err := GetFirmwareVersions(card)
		if err != nil {
			t.Errorf("Fail to get firmware version %s", err.Error())
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
		b, err := os.ReadFile(vidPath)
		vid := string(b)

		// AMD vendor ID is 0x1002
		if err == nil && strings.TrimSpace(vid) == "0x1002" {
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

func TestReadPartition(t *testing.T) {
	dir := t.TempDir()

	// RDNA has no partition files, which must not be reported as an error
	if got, err := readPartition(filepath.Join(dir, "current_compute_partition")); got != "" || err != nil {
		t.Errorf("missing file: got (%q, %v), want (\"\", nil)", got, err)
	}

	f := filepath.Join(dir, "current_memory_partition")
	if err := os.WriteFile(f, []byte("NPS1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readPartition(f); got != "nps1" || err != nil {
		t.Errorf("existing file: got (%q, %v), want (\"nps1\", nil)", got, err)
	}

	// a real read error (directory) is still reported
	if _, err := readPartition(dir); err == nil {
		t.Error("directory: want an error")
	}
}

func TestFamilyIDtoStringRDNA4(t *testing.T) {
	if got, err := FamilyIDtoString(152); err != nil || got != "GC_12_0_0" {
		t.Errorf("FamilyIDtoString(152) = %q, %v; want GC_12_0_0", got, err)
	}
}

func TestDiscoverGPUsResetsPerGPU(t *testing.T) {
	root := t.TempDir()
	mk := func(dir string, files ...string) {
		t.Helper()
		for _, f := range files {
			p := filepath.Join(root, dir, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	pci := "module/amdgpu/drivers/pci:amdgpu/"
	mk(pci+"0000:03:00.0", "numa_node", "current_compute_partition", "current_memory_partition", "drm/card0/x", "drm/renderD128/x", "drm/ttm/x")
	mk(pci+"0000:83:00.0", "numa_node", "drm/card1/x") // no render node
	mk(pci+"0000:c3:00.0", "numa_node", "drm/card2/x", "drm/renderD130/x")
	mk("devices/platform/amdgpu_xcp_1", "x") // partition without drm entries
	mk("devices/platform/amdgpu_xcp_2", "drm/renderD129/x")

	devs := discoverGPUs(root,
		map[int]string{128: "0000:03:00:0", 129: "0000:03:00:0"},
		map[int]int{128: 1, 129: 2})

	if len(devs) != 3 {
		t.Fatalf("got %d devices, want 3: %v", len(devs), devs)
	}
	if _, ok := devs["0000:83:00.0"]; ok {
		t.Error("GPU without render node must be skipped")
	}
	if d := devs["0000:c3:00.0"]; d["devID"] != "" || d["nodeId"] != 0 || d["card"] != 2 || d["renderD"] != 130 {
		t.Errorf("GPU without KFD node inherited values: %v", d)
	}
	if d := devs["amdgpu_xcp_2"]; d["devID"] != "0000:03:00:0" || d["nodeId"] != 2 || d["card"] != 0 {
		t.Errorf("partition: %v", d)
	}
}

func TestNewDeviceCapacityRejectsZeroCUs(t *testing.T) {
	if _, err := newDeviceCapacity("card0", 16<<30, 0); err == nil {
		t.Error("0 CUs: want an error")
	}
	if c, err := newDeviceCapacity("card0", 16<<30, 32); err != nil || c != (DeviceCapacity{VRAMMiB: 16384, CUCount: 32}) {
		t.Errorf("got %v, %v", c, err)
	}
}

func TestCollectFirmwareSkipsFailedQueries(t *testing.T) {
	feat, fw := collectFirmware(func(fwType uint32) (uint32, uint32, error) {
		if fwType == firmwareTypes[1].fwType {
			return 0, 0, fmt.Errorf("rc -22")
		}
		return fwType + 100, fwType, nil
	})
	if _, ok := fw["UVD"]; ok {
		t.Errorf("failed UVD query reported: %v", fw)
	}
	if _, ok := feat["UVD"]; ok {
		t.Errorf("failed UVD query reported: %v", feat)
	}
	if len(fw) != len(firmwareTypes)-1 || fw["ME"] != firmwareTypes[3].fwType+100 || feat["ME"] != firmwareTypes[3].fwType {
		t.Errorf("fw %v feat %v", fw, feat)
	}
}

func TestParseTopologyPropertiesReportsScanError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "properties")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<17)+"\nsimd_count 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTopologyProperties(path, regexp.MustCompile(`simd_count\s(\d+)`)); !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestParseDebugFSFirmwareInfoFullUint32(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amdgpu_firmware_info")
	if err := os.WriteFile(path, []byte("SOS feature version: 4294967295, firmware version: 0x80000001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	feat, fw := parseDebugFSFirmwareInfo(path)
	if feat["SOS"] != 0xffffffff || fw["SOS"] != 0x80000001 {
		t.Errorf("feat %#x fw %#x", feat["SOS"], fw["SOS"])
	}
}
