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

// AddAllocation allocates bits set in addDelta into allocation, in place.
// This matches AllocateN's second return value (delta bitmap). On error
// allocation is left unchanged.
func AddAllocation(allocation Allocation, totalCUs int, addDelta Allocation) (Allocation, error) {
	needWords := wordsFor(totalCUs)
	if len(allocation) < needWords {
		return allocation, fmt.Errorf("allocation bitmap is too short")
	}
	if len(addDelta) < needWords {
		return allocation, fmt.Errorf("add delta bitmap is too short")
	}
	for i := 0; i < needWords; i++ {
		if addDelta[i]&allocation[i] != 0 {
			return allocation, fmt.Errorf("add delta contains allocated bits")
		}
	}
	for i := 0; i < needWords; i++ {
		allocation[i] |= addDelta[i]
	}
	return allocation, nil
}

// AllocateN allocates n free CUs first-fit in whole units of unit CUs: 2 on
// RDNA, whose CU mask is applied per WGP, 1 on CDNA. n is rounded up to a
// multiple of unit, and a unit is taken only when all its CUs are free.
// Returns:
//   - updated allocation bitmap
//   - delta bitmap containing only CUs allocated in this call
//
// On success allocation is updated in place; on error it is left unchanged.
func AllocateN(allocation Allocation, totalCUs, n, unit int) (Allocation, Allocation, error) {
	if n <= 0 {
		return allocation, nil, fmt.Errorf("n must be > 0")
	}
	if unit <= 0 {
		unit = 1
	}
	if len(allocation) < wordsFor(totalCUs) {
		return allocation, nil, fmt.Errorf("allocation bitmap is too short")
	}
	n = (n + unit - 1) / unit * unit

	isSet := func(i int) bool { return allocation[i/bitsPerWord]&(uint64(1)<<uint(i%bitsPerWord)) != 0 }
	allocatedDelta := make(Allocation, len(allocation))
	allocatedCount := 0
	for start := 0; start+unit <= totalCUs && allocatedCount < n; start += unit {
		free := true
		for i := start; i < start+unit; i++ {
			free = free && !isSet(i)
		}
		if !free {
			continue
		}
		for i := start; i < start+unit; i++ {
			allocatedDelta[i/bitsPerWord] |= uint64(1) << uint(i%bitsPerWord)
		}
		allocatedCount += unit
	}

	if allocatedCount != n {
		return allocation, nil, fmt.Errorf("insufficient free CUs: need=%d free=%d", n, totalCUs-CountAllocated(allocation))
	}
	for i := range allocation {
		allocation[i] |= allocatedDelta[i]
	}
	return allocation, allocatedDelta, nil
}
