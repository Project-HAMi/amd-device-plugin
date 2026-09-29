package cuallocation

import "testing"

func TestAllocateN(t *testing.T) {
	const totalCUs = 320
	allocation, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation failed: %v", err)
	}

	allocation, delta, err := AllocateN(allocation, totalCUs, 4, 1)
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

	allocation, delta, err := AllocateN(allocation, totalCUs, 5, 1)
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
	allocation, _, err = AllocateN(allocation, totalCUs, 4, 1)
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

// isSet reports whether CU i is allocated.
func isSet(a Allocation, i int) bool {
	return a[i/bitsPerWord]&(uint64(1)<<uint(i%bitsPerWord)) != 0
}

// assertWholeWGPs fails if any WGP (CU pair) is half-enabled.
func assertWholeWGPs(t *testing.T, a Allocation, totalCUs int) {
	t.Helper()
	for i := 0; i < totalCUs; i += 2 {
		if isSet(a, i) != isSet(a, i+1) {
			t.Fatalf("WGP %d is split: CU%d=%v CU%d=%v", i/2, i, isSet(a, i), i+1, isSet(a, i+1))
		}
	}
}

// TestAllocateWGPRoundsAndAligns reproduces the RX 9060 XT case: 32 CUs, a
// request of 3 CUs on RDNA (group 2) must round up to 4 and land on CU 0-3
// (WGP0 and WGP1), never the invalid 0-2 that split WGP1.
func TestAllocateWGPRoundsAndAligns(t *testing.T) {
	const totalCUs = 32
	a, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation: %v", err)
	}
	a, delta, err := AllocateN(a, totalCUs, 3, 2)
	if err != nil {
		t.Fatalf("AllocateN: %v", err)
	}
	if CountAllocated(delta) != 4 {
		t.Fatalf("request 3 with group 2 should allocate 4 CUs, got %d", CountAllocated(delta))
	}
	for i := 0; i < 4; i++ {
		if !isSet(a, i) {
			t.Fatalf("CU%d should be allocated", i)
		}
	}
	assertWholeWGPs(t, a, totalCUs)
}

// TestAllocateWGPTwoContainers checks a second slice on the same GPU starts on
// a WGP boundary, so neither container splits a WGP.
func TestAllocateWGPTwoContainers(t *testing.T) {
	const totalCUs = 32
	a, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation: %v", err)
	}
	a, d1, err := AllocateN(a, totalCUs, 3, 2) // -> CU 0-3
	if err != nil {
		t.Fatalf("first AllocateN: %v", err)
	}
	a, d2, err := AllocateN(a, totalCUs, 3, 2) // -> CU 4-7
	if err != nil {
		t.Fatalf("second AllocateN: %v", err)
	}
	if CountAllocated(d1) != 4 || CountAllocated(d2) != 4 {
		t.Fatalf("each slice should be 4 CUs, got %d and %d", CountAllocated(d1), CountAllocated(d2))
	}
	if d1[0]&d2[0] != 0 {
		t.Fatalf("slices overlap: %064b vs %064b", d1[0], d2[0])
	}
	assertWholeWGPs(t, a, totalCUs)
}

// TestAllocateWGPInsufficient checks a request that cannot fit in whole WGPs
// fails instead of emitting a partial or invalid mask.
func TestAllocateWGPInsufficient(t *testing.T) {
	const totalCUs = 4
	a, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation: %v", err)
	}
	if _, _, err := AllocateN(a, totalCUs, 5, 2); err == nil {
		t.Fatal("expected error when the rounded request exceeds the device")
	}
}

// TestAllocateGroupOneUnchanged checks CDNA behaviour (group 1) still does
// plain single-CU first-fit.
func TestAllocateGroupOneUnchanged(t *testing.T) {
	const totalCUs = 64
	a, err := NewAllocation(totalCUs)
	if err != nil {
		t.Fatalf("NewAllocation: %v", err)
	}
	a, delta, err := AllocateN(a, totalCUs, 3, 1)
	if err != nil {
		t.Fatalf("AllocateN: %v", err)
	}
	if CountAllocated(delta) != 3 {
		t.Fatalf("group 1 request 3 should allocate exactly 3, got %d", CountAllocated(delta))
	}
}

// TestGroupAligned checks the WGP validator: whole pairs pass, a split pair
// fails, and group 1 always passes.
func TestGroupAligned(t *testing.T) {
	const totalCUs = 8
	a, _ := NewAllocation(totalCUs)
	a[0] = 0b1111 // CU 0-3: WGP0, WGP1 whole
	if !GroupAligned(a, totalCUs, 2) {
		t.Fatal("0-3 should be group-aligned for group 2")
	}
	split, _ := NewAllocation(totalCUs)
	split[0] = 0b0111 // CU 0-2: WGP1 (CU2,CU3) half-enabled
	if GroupAligned(split, totalCUs, 2) {
		t.Fatal("0-2 splits WGP1 and must not be group-aligned")
	}
	if !GroupAligned(split, totalCUs, 1) {
		t.Fatal("group 1 is always aligned")
	}
}

// TestAllocateNeverSplitsWGP checks that no request produces a group-split
// mask on RDNA (group 2), across a range of odd and even CU counts.
func TestAllocateNeverSplitsWGP(t *testing.T) {
	const totalCUs = 32
	for n := 1; n <= totalCUs; n++ {
		a, _ := NewAllocation(totalCUs)
		_, delta, err := AllocateN(a, totalCUs, n, 2)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !GroupAligned(delta, totalCUs, 2) {
			t.Fatalf("n=%d produced a WGP-splitting mask", n)
		}
	}
}
