/**
# Copyright 2025 Advanced Micro Devices, Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the \"License\");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an \"AS IS\" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package allocator

import "fmt"

type Policy interface {
	Init(devs []*Device, topoDir string) error
	Allocate(available, required []string, size int) ([]string, error)
}

// NewPolicy returns the allocation policy with the given name. binpack packs
// devices onto the closest ones, which is what besteffort already scores.
func NewPolicy(name string) (Policy, error) {
	switch name {
	case "", "besteffort", "binpack":
		return NewBestEffortPolicy(), nil
	case "spread":
		return NewSpreadPolicy(), nil
	}
	return nil, fmt.Errorf("unknown allocator policy %q, want besteffort, binpack or spread", name)
}
