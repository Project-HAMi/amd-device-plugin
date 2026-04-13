package cuallocation

import "testing"

func TestAllocateN(t *testing.T) {
	const totalCUs = 320
	allocation, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation failed: %v", err)
	}

	allocation, delta, err := AllocateN(allocation, totalCUs, 4)
	if err != nil {
		t.Fatalf("AllocateN failed: %v", err)
	}
	if CountAllocated(allocation) != 4 {
		t.Fatalf("expected 4 allocated, got %d", CountAllocated(allocation))
	}
	if CountAllocated(delta) != 4 {
		t.Fatalf("expected 4 delta allocated, got %d", CountAllocated(delta))
	}
}

func TestReleaseManyWithDelta(t *testing.T) {
	const totalCUs = 320
	allocation, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation failed: %v", err)
	}

	allocation, delta, err := AllocateN(allocation, totalCUs, 5)
	if err != nil {
		t.Fatalf("AllocateN failed: %v", err)
	}
	if CountAllocated(allocation) != 5 {
		t.Fatalf("expected 5 allocated, got %d", CountAllocated(allocation))
	}

	allocation, err = ReleaseMany(allocation, totalCUs, delta)
	if err != nil {
		t.Fatalf("ReleaseMany failed: %v", err)
	}
	if CountAllocated(allocation) != 0 {
		t.Fatalf("expected 0 allocated after release, got %d", CountAllocated(allocation))
	}
}

