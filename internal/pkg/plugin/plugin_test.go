/**
 * Copyright 2018 Advanced Micro Devices, Inc.  All rights reserved.
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

package plugin

import (
	"testing"

	"github.com/ROCm/k8s-device-plugin/internal/pkg/cuallocation"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCountGPUDevFromTopology(t *testing.T) {
	count := countGPUDevFromTopology("../../../testdata/topology-parsing")

	expCount := 2
	if count != expCount {
		t.Errorf("Count was incorrect, got: %d, want: %d.", count, expCount)
	}
}

func TestTerminalPodReleasesCUAllocation(t *testing.T) {
	const uuid = "node~gpu0"
	p := &AMDGPUPlugin{
		deviceCache:  []*utils.DeviceInfo{{ID: uuid, Devcore: 8}},
		cuAllocation: make(map[string]cuallocation.Allocation),
	}
	if err := p.initCuAllocationForDevices(p.deviceCache); err != nil {
		t.Fatalf("initialize CU allocation: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
			Annotations: map[string]string{
				utils.CuAllocation: `{"node~gpu0":"0-3"}`,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	p.onPodAdd(pod)
	assertCUAllocationWord(t, p, uuid, 0x0f)

	completed := pod.DeepCopy()
	completed.Status.Phase = corev1.PodSucceeded
	p.onPodUpdate(pod, completed)
	assertCUAllocationWord(t, p, uuid, 0)

	// Repeated terminal updates and the eventual delete event must not add or
	// release the same bitmap again.
	p.onPodUpdate(completed, completed)
	p.onPodDelete(completed)
	assertCUAllocationWord(t, p, uuid, 0)
}

func TestTerminalPodIsIgnoredWhenRebuildingCUAllocation(t *testing.T) {
	const uuid = "node~gpu0"
	p := &AMDGPUPlugin{
		deviceCache:  []*utils.DeviceInfo{{ID: uuid, Devcore: 8}},
		cuAllocation: make(map[string]cuallocation.Allocation),
	}
	if err := p.initCuAllocationForDevices(p.deviceCache); err != nil {
		t.Fatalf("initialize CU allocation: %v", err)
	}

	p.onPodAdd(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			utils.CuAllocation: `{"node~gpu0":"0-3"}`,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	})
	assertCUAllocationWord(t, p, uuid, 0)
}

func assertCUAllocationWord(t *testing.T, p *AMDGPUPlugin, uuid string, want uint64) {
	t.Helper()
	allocation, ok := p.getDeviceAllocation(uuid)
	if !ok || len(allocation) == 0 {
		t.Fatalf("allocation for %s is missing", uuid)
	}
	if allocation[0] != want {
		t.Fatalf("allocation word for %s = %#x, want %#x", uuid, allocation[0], want)
	}
}
