package amdgpu

import "testing"

func TestFamilyFromProduct(t *testing.T) {
	cases := map[string]string{
		"AMD Instinct MI355X": "gfx950",
		"AMD Instinct MI300X": "gfx942",
		"AMD Instinct MI210":  "gfx90a",
		"AMD Instinct MI250":  "gfx90a",
		"unknown vendor part": "gfx950",
	}
	for product, want := range cases {
		if got := familyFromProduct(product); got != want {
			t.Errorf("familyFromProduct(%q) = %q, want %q", product, got, want)
		}
	}
}

// Every combo's memory mode must have a physical count in the NPS table.
func TestCombosResolveInNPSMemoryTable(t *testing.T) {
	for family, combos := range partitionCombos {
		table := NPSMemoryTable[family]
		if table == nil {
			t.Errorf("family %q missing from NPSMemoryTable", family)
			continue
		}
		for _, combo := range combos {
			if table[combo.Memory] == 0 {
				t.Errorf("family %q combo %s/%s: memory mode %q missing from NPSMemoryTable",
					family, combo.Compute, combo.Memory, combo.Memory)
			}
		}
	}
}
