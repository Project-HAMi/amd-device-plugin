/*
Copyright 2024 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func lockOf(ns, name string) string { return "2026-01-01T00:00:00Z," + ns + "," + name }

func setup(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	old := KubeClient
	t.Cleanup(func() { KubeClient = old })
	KubeClient = cs
	return cs
}

func lockedNode(owner string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Annotations: map[string]string{NodeLockKey: lockOf("ns", owner)}}}
}

func testPod(toAllocate string, ctrs ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: map[string]string{
		BindTimeAnnotations:     "1",
		AssignedNodeAnnotations: "n",
		DeviceBindPhase:         DeviceBindAllocating,
		DeviceToAllocate:        toAllocate,
	}}, Spec: corev1.PodSpec{NodeName: "n"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	for _, c := range ctrs {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

func nodeLock(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), "n", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n.Annotations[NodeLockKey]
}

// The lock is held until every container's devices are allocated, then released.
func TestPodAllocationTrySuccess(t *testing.T) {
	for _, tc := range []struct {
		name, remaining string
		released        bool
	}{
		{"second container pending", ";uuid-b,AMDGPU,1024,10:;", false},
		{"all erased", ";;", true},
		{"annotation empty", "", true},
	} {
		pod := testPod(tc.remaining)
		pod.Annotations[DeviceAllocation] = "uuid-a,AMDGPU,1024,10:;uuid-b,AMDGPU,1024,10:;"
		cs := setup(t, lockedNode("p"), pod)
		PodAllocationTrySuccess("n", "lock", pod)
		if got := nodeLock(t, cs) == ""; got != tc.released {
			t.Errorf("%s: released = %v, want %v", tc.name, got, tc.released)
		}
	}
}

// A failed phase patch must not leave the node locked.
func TestPodAllocationFailedReleasesLockWhenPatchFails(t *testing.T) {
	cs := setup(t, lockedNode("p")) // pod missing, so the patch fails
	PodAllocationFailed("n", testPod(""), "lock")
	if l := nodeLock(t, cs); l != "" {
		t.Errorf("node lock %q kept after failed pod patch", l)
	}
}

// The lock can change hands between retries; the new owner's lock must stay.
func TestReleaseNodeLockRechecksOwnerOnRetry(t *testing.T) {
	cs := setup(t, lockedNode("p"))
	first := true
	cs.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !first {
			return false, nil, nil
		}
		first = false
		if err := cs.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, lockedNode("other"), ""); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "n", nil)
	})
	if err := ReleaseNodeLock("n", testPod(""), false); err != nil {
		t.Fatal(err)
	}
	if l := nodeLock(t, cs); l != lockOf("ns", "other") {
		t.Errorf("node lock = %q, want the other pod's lock kept", l)
	}
}

// A deleted lock owner falls back to scanning pods on the node.
func TestGetPendingPodLockOwnerDeleted(t *testing.T) {
	setup(t, lockedNode("gone"), testPod(""))
	pod, err := GetPendingPod(context.Background(), "n")
	if err != nil || pod == nil || pod.Name != "p" {
		t.Fatalf("GetPendingPod = %v, %v; want pod p", pod, err)
	}
}

// Entries stay aligned with Spec.Containers when a GPU-less container comes first.
func TestNextDeviceRequestSkipsGPULessContainer(t *testing.T) {
	pod := testPod(";uuid-a,AMDGPU,4096,25:;uuid-b,AMDGPU,4096,25:;", "sidecar", "gpu1", "gpu2")
	cs := setup(t, pod)
	for _, want := range []string{"gpu1", "gpu2"} {
		cur, err := cs.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctr, _, err := GetNextDeviceRequest("amd", *cur)
		if err != nil || ctr.Name != want {
			t.Fatalf("GetNextDeviceRequest = %q, %v; want %q", ctr.Name, err, want)
		}
		if err := EraseNextDeviceTypeFromAnnotation("amd", *cur); err != nil {
			t.Fatal(err)
		}
	}
}

// An annotation naming a container the pod does not have is an error, not a panic.
func TestNextDeviceRequestRejectsMissingContainer(t *testing.T) {
	pod := testPod(";uuid-a,AMDGPU,4096,25:;", "only")
	if _, _, err := GetNextDeviceRequest("amd", *pod); err == nil {
		t.Error("request for container 1 of a one-container pod accepted")
	}
}

func TestPodDevicesRoundTrip(t *testing.T) {
	for _, s := range []string{";", ";;", "uuid-a,AMDGPU,4096,25:;", ";uuid-a,AMDGPU,4096,25:uuid-b,AMDGPU,1,2:;;"} {
		pd, err := DecodePodDevices(InRequestDevices, map[string]string{DeviceToAllocate: s})
		if err != nil {
			t.Fatal(err)
		}
		if got := EncodePodSingleDevice(pd["amd"]); got != s {
			t.Errorf("round trip of %q = %q", s, got)
		}
	}
}

func TestDecodePodDevicesMalformed(t *testing.T) {
	for _, s := range []string{"uuid-a,AMDGPU,x,25:;", "uuid-a,AMDGPU,4096,x:;"} {
		if _, err := DecodePodDevices(InRequestDevices, map[string]string{DeviceToAllocate: s}); err == nil {
			t.Errorf("DecodePodDevices(%q) returned no error", s)
		}
	}
}
