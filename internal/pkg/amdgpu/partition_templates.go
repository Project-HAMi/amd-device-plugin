package amdgpu

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
)

// PartitionTemplate is one apply-ready combination for a GPU: the compute
// partition style, the NPS memory mode, and the physical memory partition
// count that mode exposes (1/4/8 HBM stacks on gfx950-class cards).
type PartitionTemplate struct {
	Compute        string `json:"computePartition"`
	Memory         string `json:"memoryPartition"`
	PhysicalMemory int    `json:"physicalMemory"`
	Current        bool   `json:"current"`
}

// DevicePartitionTemplates is the boot-time template catalog for one GPU.
type DevicePartitionTemplates struct {
	BDF                     string              `json:"bdf"`
	Product                 string              `json:"product"`
	CurrentComputePartition string              `json:"currentComputePartition"`
	CurrentMemoryPartition  string              `json:"currentMemoryPartition"`
	CurrentPhysicalMemory   int                 `json:"currentPhysicalMemory"`
	NumANode                int                 `json:"numaNode"`
	CUCount                 int32               `json:"cuCount"`
	VRAMMiB                 int32               `json:"vramMiB"`
	Templates               []PartitionTemplate `json:"templates"`
}

// NPSMemoryTable maps the physical memory partition count each NPS mode
// exposes, per product family. This is the lookup table used to translate an
// NPS mode into memory domains for scheduling and Allocate.
// ponytail: counts are for the MI300/MI355 class; add new families here
// before scheduling a new generation.
var NPSMemoryTable = map[string]map[string]int{
	"gfx950": {"nps1": 1, "nps4": 4, "nps8": 8},
	"gfx942": {"nps1": 1, "nps4": 4, "nps8": 8},
	"gfx90a": {"nps1": 1, "nps2": 2, "nps4": 4},
}

// partitionCombos lists the valid compute x memory combinations per family.
// Compute styles: spx (single), dpx (dual), qpx (quad partition).
// ponytail: firmware-gated; unlisted combos simply get no template.
var partitionCombos = map[string][]struct {
	Compute, Memory string
}{
	"gfx950": {
		{"spx", "nps1"}, {"spx", "nps4"}, {"spx", "nps8"},
		{"dpx", "nps1"}, {"dpx", "nps4"},
		{"qpx", "nps1"},
	},
	"gfx942": {
		{"spx", "nps1"}, {"spx", "nps4"}, {"spx", "nps8"},
		{"dpx", "nps1"}, {"dpx", "nps4"},
		{"qpx", "nps1"},
	},
	"gfx90a": {
		{"spx", "nps1"}, {"spx", "nps2"}, {"spx", "nps4"},
		{"dpx", "nps1"}, {"dpx", "nps2"},
	},
}

func familyFromProduct(product string) string {
	p := strings.ToLower(product)
	switch {
	case strings.Contains(p, "mi355"), strings.Contains(p, "gfx950"):
		return "gfx950"
	case strings.Contains(p, "mi300"), strings.Contains(p, "gfx942"):
		return "gfx942"
	case strings.Contains(p, "mi210"), strings.Contains(p, "mi250"), strings.Contains(p, "gfx90a"):
		return "gfx90a"
	}
	// ponytail: unknown products default to the gfx950 table; add the family
	// above before scheduling a new generation.
	return "gfx950"
}

// GeneratePartitionTemplates builds the per-card template catalog from live
// sysfs partition state and the product-family tables. Each card gets one
// entry per valid combination, with the currently active one flagged.
func GeneratePartitionTemplates() ([]DevicePartitionTemplates, error) {
	gpus := GetAMDGPUs()
	bdfs := make([]string, 0, len(gpus))
	for _, data := range gpus {
		if bdf, ok := data["devID"].(string); ok {
			bdfs = append(bdfs, bdf)
		}
	}
	products, err := GetAMDSMIProductNames(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI product lookup incomplete; templates fall back to family defaults: %v", err)
	}

	var out []DevicePartitionTemplates
	for key, data := range gpus {
		compute, _ := data["computePartitionType"].(string)
		memory, _ := data["memoryPartitionType"].(string)
		if compute == "" {
			compute = "spx"
		}
		if memory == "" {
			memory = "nps1"
		}
		product := ""
		if bdf, ok := data["devID"].(string); ok {
			product = products[strings.ToLower(bdf)]
		}
		family := familyFromProduct(product)
		memoryTable := NPSMemoryTable[family]
		combos := partitionCombos[family]

		card, _ := data["card"].(int)
		numa, _ := data["numaNode"].(int)
		capacity, _ := GetDeviceCapacity(fmt.Sprintf("card%d", card))

		entry := DevicePartitionTemplates{
			BDF:                     key,
			Product:                 product,
			CurrentComputePartition: compute,
			CurrentMemoryPartition:  memory,
			CurrentPhysicalMemory:   memoryTable[memory],
			NumANode:                numa,
			CUCount:                 capacity.CUCount,
			VRAMMiB:                 capacity.VRAMMiB,
		}
		for _, combo := range combos {
			entry.Templates = append(entry.Templates, PartitionTemplate{
				Compute:        combo.Compute,
				Memory:         combo.Memory,
				PhysicalMemory: memoryTable[combo.Memory],
				Current:        combo.Compute == compute && combo.Memory == memory,
			})
		}
		out = append(out, entry)
	}
	return out, nil
}

