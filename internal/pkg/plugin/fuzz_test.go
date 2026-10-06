/*
 * Copyright 2026 The HAMi Authors.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package plugin

import (
	"slices"
	"testing"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
)

// The CU list comes back from the hami.io/amd-cu-allocated pod annotation, so
// parsing must never panic, and a list it accepts must survive a round trip.
func FuzzIDListToAllocation(f *testing.F) {
	for _, seed := range []string{"0-3,8", "0", "", "16-31", " 1 , 3-5 ", "5-2", "-1", "0-63,64-127", "a-b", "1--2"} {
		f.Add(seed, 64)
		f.Add(seed, 128)
	}
	f.Fuzz(func(t *testing.T, s string, totalCUs int) {
		if totalCUs <= 0 || totalCUs > 1024 {
			return
		}
		a, err := idListToAllocation(s, totalCUs)
		if err != nil || cuallocation.CountAllocated(a) == 0 {
			return
		}
		b, err := idListToAllocation(allocationToIDList(a, totalCUs), totalCUs)
		if err != nil || !slices.Equal(a, b) {
			t.Fatalf("round trip of %q (totalCUs=%d) = %v, %v; want %v", s, totalCUs, b, err, a)
		}
	})
}
