package cuallocation

import (
	"fmt"
	"math/bits"
)

const bitsPerWord = 64

// Allocation uses a segmented bitmap to store the allocation state of CUs.
type Allocation []uint64

func wordsFor(totalCUs int) int {
	if totalCUs <= 0 {
		return 0
	}
	return (totalCUs + bitsPerWord - 1) / bitsPerWord
}

// NewAllocation creates an empty bitmap for totalCUs CUs.
func NewAllocation(totalCUs int) (Allocation, error) {
	if totalCUs <= 0 {
		return nil, fmt.Errorf("totalCUs must be > 0")
	}
	return make(Allocation, wordsFor(totalCUs)), nil
}

// CountAllocated returns the number of allocated CUs.
func CountAllocated(allocation Allocation) int {
	count := 0
	for _, w := range allocation {
		count += bits.OnesCount64(w)
	}
	return count
}

// AddAllocation allocates bits set in addDelta into allocation.
// This matches AllocateN's second return value (delta bitmap).
func AddAllocation(allocation Allocation, totalCUs int, addDelta Allocation) (Allocation, error) {
	needWords := wordsFor(totalCUs)
	if len(allocation) < needWords {
		return allocation, fmt.Errorf("allocation bitmap is too short")
	}
	if len(addDelta) < needWords {
		return allocation, fmt.Errorf("add delta bitmap is too short")
	}

	current := allocation
	for i := 0; i < needWords; i++ {
		// addDelta cannot contain bits that are already allocated.
		if addDelta[i]&current[i] != 0 {
			return allocation, fmt.Errorf("add delta contains allocated bits")
		}
		current[i] |= addDelta[i]
	}
	return current, nil
}

// AllocateN allocates free CUs in ascending index order (first-fit), rounding
// the request up to a whole number of groups and aligning every group to a
// group boundary. group is the number of CUs that must be enabled together: 1
// on CDNA, where a CU is masked on its own, and 2 on RDNA, where a WGP is a
// pair of CUs and a mask that enables a single CU of the pair is invalid. A
// group is taken only when all of its CUs are free, so no group is ever split.
// Returns:
//   - updated allocation bitmap
//   - delta bitmap containing only CUs allocated in this call
func AllocateN(allocation Allocation, totalCUs, n, group int) (Allocation, Allocation, error) {
	if n <= 0 {
		return allocation, nil, fmt.Errorf("n must be > 0")
	}
	if group <= 0 {
		group = 1
	}
	if len(allocation) < wordsFor(totalCUs) {
		return allocation, nil, fmt.Errorf("allocation bitmap is too short")
	}

	// Round up to whole groups: a request that lands mid-group takes the rest
	// of that group too, so the emitted mask only ever holds whole groups.
	need := ((n + group - 1) / group) * group
	if need > totalCUs {
		return allocation, nil, fmt.Errorf("request of %d CUs rounds up to %d, more than the %d CUs on the device", n, need, totalCUs)
	}

	allocatedDelta := make(Allocation, len(allocation))
	allocatedCount := 0
	for start := 0; start+group <= totalCUs && allocatedCount < need; start += group {
		free := true
		for j := 0; j < group; j++ {
			word := (start + j) / bitsPerWord
			bit := uint((start + j) % bitsPerWord)
			if allocation[word]&(uint64(1)<<bit) != 0 {
				free = false
				break
			}
		}
		if !free {
			continue
		}
		for j := 0; j < group; j++ {
			word := (start + j) / bitsPerWord
			bit := uint((start + j) % bitsPerWord)
			allocation[word] |= uint64(1) << bit
			allocatedDelta[word] |= uint64(1) << bit
		}
		allocatedCount += group
	}

	if allocatedCount != need {
		return allocation, nil, fmt.Errorf("insufficient free CUs: need=%d (request %d, group %d) free=%d", need, n, group, totalCUs-CountAllocated(allocation))
	}
	return allocation, allocatedDelta, nil
}

// ReleaseAllocation deallocates bits set in releaseDelta from allocation.
// This matches AllocateN's second return value (delta bitmap).
func ReleaseAllocation(allocation Allocation, totalCUs int, releaseDelta Allocation) (Allocation, error) {
	needWords := wordsFor(totalCUs)
	if len(allocation) < needWords {
		return allocation, fmt.Errorf("allocation bitmap is too short")
	}
	if len(releaseDelta) < needWords {
		return allocation, fmt.Errorf("release delta bitmap is too short")
	}

	current := allocation
	for i := 0; i < needWords; i++ {
		// releaseDelta cannot contain bits that are not currently allocated.
		if releaseDelta[i]&^current[i] != 0 {
			return allocation, fmt.Errorf("release delta contains unallocated bits")
		}
		current[i] &^= releaseDelta[i]
	}
	return current, nil
}

// GroupAligned reports whether the allocation splits no group: every allocated
// CU has the other CUs of its group allocated too. On RDNA a group is a WGP (a
// CU pair), and a mask that enables a single CU of the pair is invalid, so the
// emitted HSA_CU_MASK must pass this check. group <= 1 is always aligned.
func GroupAligned(allocation Allocation, totalCUs, group int) bool {
	if group <= 1 {
		return true
	}
	for start := 0; start < totalCUs; start += group {
		any, all := false, true
		for j := 0; j < group && start+j < totalCUs; j++ {
			word := (start + j) / bitsPerWord
			bit := uint((start + j) % bitsPerWord)
			set := allocation[word]&(uint64(1)<<bit) != 0
			any = any || set
			all = all && set
		}
		if any && !all {
			return false
		}
	}
	return true
}
