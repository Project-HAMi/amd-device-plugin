/**
 * Copyright 2021 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

// Package hwloc is a collection of utility functions to get NUMA membership
// of AMD GPU via the hwloc library
package hwloc

// #cgo pkg-config: hwloc
// #include <stdint.h>
// #include <hwloc.h>
import "C"
import (
	"fmt"
)

func GetVersions() string {
	return fmt.Sprintf("hwloc: _VERSION: %s, _API_VERSION: %#08x, _COMPONENT_ABI: %d, Runtime: %#08x",
		C.HWLOC_VERSION,
		C.HWLOC_API_VERSION,
		C.HWLOC_COMPONENT_ABI,
		uint(C.hwloc_get_api_version()))
}
