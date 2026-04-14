package cuallocation

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
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

// AllocateN allocates n free CUs in ascending index order (first-fit).
// Returns:
//   - updated allocation bitmap
//   - delta bitmap containing only CUs allocated in this call
func AllocateN(allocation Allocation, totalCUs int, n int) (Allocation, Allocation, error) {
	if n <= 0 {
		return allocation, nil, fmt.Errorf("n must be > 0")
	}
	if len(allocation) < wordsFor(totalCUs) {
		return allocation, nil, fmt.Errorf("allocation bitmap is too short")
	}

	allocatedDelta := make(Allocation, len(allocation))
	allocatedCount := 0
	for i := 0; i < totalCUs && allocatedCount < n; i++ {
		word := i / bitsPerWord
		bit := uint(i % bitsPerWord)
		mask := uint64(1) << bit
		if allocation[word]&mask != 0 {
			continue
		}
		allocation[word] |= mask
		allocatedDelta[word] |= mask
		allocatedCount++
	}

	if allocatedCount != n {
		return allocation, nil, fmt.Errorf("insufficient free CUs: need=%d free=%d", n, totalCUs-CountAllocated(allocation))
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

func ConvertAllocationToHex(allocation Allocation) string {
	hex := ""
	for _, word := range allocation {
		hex += fmt.Sprintf("%016X", word)
	}
	return hex
}

func ConvertHexToAllocation(hex string) (Allocation, error) {
	s := strings.TrimSpace(hex)
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if s == "" {
		return nil, fmt.Errorf("empty hex allocation")
	}
	// left-pad to 16-hex alignment
	if rem := len(s) % 16; rem != 0 {
		s = strings.Repeat("0", 16-rem) + s
	}

	allocation := make(Allocation, len(s)/16)
	for i := 0; i < len(s); i += 16 {
		word, err := strconv.ParseUint(s[i:i+16], 16, 64)
		if err != nil {
			return nil, err
		}
		allocation[i/16] = word
	}
	return allocation, nil
}