package amdsmi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
)

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

func TestNormalizeBDF(t *testing.T) {
	for _, input := range []string{"0000:83:00.0", "0000:83:00:0"} {
		if got, want := normalizeBDF(input), "0000:83:00.0"; got != want {
			t.Fatalf("normalizeBDF(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAMDSMICacheReturnsOnlyRequestedOnError(t *testing.T) {
	c := newAMDSCache(func(bdfs []string) (map[string]string, error) {
		return map[string]string{"a": "1"}, fmt.Errorf("b failed")
	})
	c.Get([]string{"a", "b"})
	got, err := c.Get([]string{"c"})
	if err == nil || len(got) != 0 {
		t.Fatalf("Get(c) = %v, %v; want empty map and an error", got, err)
	}
}

// GPUs without accelerator partitions (RDNA) must fail the partition lookups
// cleanly, keep nothing and not abort.
func TestPartitionLookupsWithoutPartitionSupport(t *testing.T) {
	if !hasAMDGPU() {
		t.Skip("Skipping test, no AMD GPU found.")
	}
	var bdfs []string
	for _, d := range amdgpu.GetAMDGPUs() {
		if d["computePartitionType"] != "" {
			t.Skip("GPU supports compute partitions")
		}
		bdfs = append(bdfs, d["devID"].(string))
	}
	if got, err := GetAMDSCurrentMemoryPartitions(bdfs); err == nil || len(got) != 0 {
		t.Errorf("memory partitions = %v, %v; want empty and an error", got, err)
	}
	if got, err := GetAMDGPUPartitionProfiles(bdfs); err == nil || len(got) != 0 {
		t.Errorf("partition profiles = %v, %v; want empty and an error", got, err)
	}
	if err := SetGPUComputePartitions(bdfs, "qpx"); err == nil {
		t.Error("flipping a GPU without partition profiles succeeded")
	}
	if err := SetGPUComputePartitions(nil, "qpx"); err != nil {
		t.Errorf("empty flip = %v, want nil", err)
	}
}

// AMD SMI listed the XCC count for the current profile (SPX) only; the others
// follow from the total split over their partitions.
func TestFillXCCPerPartition(t *testing.T) {
	profiles := []PartitionProfile{
		{Type: "SPX", NumPartitions: 1, XCCPerPartition: 8},
		{Type: "DPX", NumPartitions: 2},
		{Type: "QPX", NumPartitions: 4},
		{Type: "CPX", NumPartitions: 8},
	}
	fillXCCPerPartition(profiles)
	for i, want := range []int{8, 4, 2, 1} {
		if profiles[i].XCCPerPartition != want {
			t.Errorf("%s: XCCPerPartition = %d, want %d", profiles[i].Type, profiles[i].XCCPerPartition, want)
		}
	}

	// a value AMD SMI did give is kept, and without a total there is nothing to derive
	kept := []PartitionProfile{{Type: "SPX", NumPartitions: 1, XCCPerPartition: 8}, {Type: "DPX", NumPartitions: 2, XCCPerPartition: 3}}
	fillXCCPerPartition(kept)
	if kept[1].XCCPerPartition != 3 {
		t.Errorf("an XCC count from AMD SMI was overwritten: %d", kept[1].XCCPerPartition)
	}
	none := []PartitionProfile{{Type: "DPX", NumPartitions: 2}}
	fillXCCPerPartition(none)
	if none[0].XCCPerPartition != 0 {
		t.Errorf("no total, got %d", none[0].XCCPerPartition)
	}
}
