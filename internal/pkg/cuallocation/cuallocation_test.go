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

	allocation, err = ReleaseAllocation(allocation, totalCUs, delta)
	if err != nil {
		t.Fatalf("ReleaseMany failed: %v", err)
	}
	if CountAllocated(allocation) != 0 {
		t.Fatalf("expected 0 allocated after release, got %d", CountAllocated(allocation))
	}
}

func TestAddAllocationWithDelta(t *testing.T) {
	const totalCUs = 128
	allocation, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation failed: %v", err)
	}

	addDelta := make(Allocation, len(allocation))
	addDelta[0] = (uint64(1) << 10) | (uint64(1) << 11)

	allocation, err = AddAllocation(allocation, totalCUs, addDelta)
	if err != nil {
		t.Fatalf("AddAllocation failed: %v", err)
	}
	if CountAllocated(allocation) != 2 {
		t.Fatalf("expected 2 allocated after add, got %d", CountAllocated(allocation))
	}
}

func TestUpdateByReleaseThenAdd(t *testing.T) {
	const totalCUs = 128
	allocation, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation failed: %v", err)
	}

	// Initial allocation: CU 0-3.
	allocation, _, err = AllocateN(allocation, totalCUs, 4)
	if err != nil {
		t.Fatalf("AllocateN failed: %v", err)
	}

	// Update target: keep CU 2-3, replace CU 0-1 with CU 4-5.
	releaseDelta := make(Allocation, len(allocation))
	releaseDelta[0] = (uint64(1) << 0) | (uint64(1) << 1)
	addDelta := make(Allocation, len(allocation))
	addDelta[0] = (uint64(1) << 4) | (uint64(1) << 5)

	allocation, err = ReleaseAllocation(allocation, totalCUs, releaseDelta)
	if err != nil {
		t.Fatalf("ReleaseMany failed: %v", err)
	}
	allocation, err = AddAllocation(allocation, totalCUs, addDelta)
	if err != nil {
		t.Fatalf("AddAllocation failed: %v", err)
	}

	expected := (uint64(1) << 2) | (uint64(1) << 3) | (uint64(1) << 4) | (uint64(1) << 5)
	if allocation[0] != expected {
		t.Fatalf("unexpected bitmap after update: got=%064b want=%064b", allocation[0], expected)
	}
}

