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

package dmem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestAvailable(t *testing.T) {
	withController := t.TempDir()
	if err := os.WriteFile(filepath.Join(withController, "cgroup.controllers"), []byte("cpuset cpu io memory hugetlb pids rdma misc dmem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Available(withController) {
		t.Error("dmem listed in cgroup.controllers should report available")
	}

	withoutController := t.TempDir()
	if err := os.WriteFile(filepath.Join(withoutController, "cgroup.controllers"), []byte("cpuset cpu io memory hugetlb pids rdma misc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Available(withoutController) {
		t.Error("dmem absent from cgroup.controllers should report unavailable")
	}

	if Available(t.TempDir()) {
		t.Error("missing cgroup.controllers should report unavailable, not panic")
	}
}

func TestRegion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dmem.capacity"), []byte("drm/0000:06:00.0/vram 17095983104\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if region, ok := Region(root, "0000:06:00.0"); !ok || region != "drm/0000:06:00.0/vram" {
		t.Errorf("Region(registered bdf) = %q, %v; want drm/0000:06:00.0/vram, true", region, ok)
	}
	// RenderD/KFD topology sometimes spells the BDF uppercase; match case-insensitively.
	if _, ok := Region(root, "0000:06:00.0"); !ok {
		t.Error("Region should match case-insensitively")
	}
	if _, ok := Region(root, "0000:99:00.0"); ok {
		t.Error("Region(unregistered bdf) should report false")
	}
	if _, ok := Region(t.TempDir(), "0000:06:00.0"); ok {
		t.Error("missing dmem.capacity should report false, not panic")
	}
}

func TestPodCgroupPath(t *testing.T) {
	const uid = "06076004-3cf7-4399-bffd-d7c3596543b3"
	const root = "/sys/fs/cgroup"

	for _, tc := range []struct {
		qos  corev1.PodQOSClass
		want string
	}{
		{corev1.PodQOSGuaranteed, "/sys/fs/cgroup/kubepods.slice/kubepods-pod06076004_3cf7_4399_bffd_d7c3596543b3.slice"},
		{corev1.PodQOSBurstable, "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod06076004_3cf7_4399_bffd_d7c3596543b3.slice"},
		{corev1.PodQOSBestEffort, "/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod06076004_3cf7_4399_bffd_d7c3596543b3.slice"},
	} {
		if got := PodCgroupPath(root, uid, tc.qos); got != tc.want {
			t.Errorf("PodCgroupPath(%s) = %q, want %q", tc.qos, got, tc.want)
		}
	}
}

func TestSetMax(t *testing.T) {
	dir := t.TempDir()
	// dmem.max must already exist, like a real cgroup control file.
	target := filepath.Join(dir, "dmem.max")
	if err := os.WriteFile(target, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetMax(dir, "drm/0000:06:00.0/vram", 2147483648); err != nil {
		t.Fatalf("SetMax: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := "drm/0000:06:00.0/vram 2147483648\n"; string(got) != want {
		t.Errorf("dmem.max content = %q, want %q", got, want)
	}

	if err := SetMax(filepath.Join(dir, "missing"), "drm/0000:06:00.0/vram", 1); err == nil {
		t.Error("SetMax against a nonexistent pod cgroup should error, not silently succeed")
	}
}

func TestNormalizeBDF(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0000:06:00:0", "0000:06:00.0"},
		{"0000:06:00.0", "0000:06:00.0"},
		{"not-a-bdf", "not-a-bdf"},
	} {
		if got := NormalizeBDF(tc.in); got != tc.want {
			t.Errorf("NormalizeBDF(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetMaxWithRetryWaitsForCgroup(t *testing.T) {
	// The pod cgroup DIRECTORY, not just dmem.max, does not exist yet when
	// Allocate runs a moment too early; os.WriteFile to a path under a
	// missing directory fails with ENOENT, same as a missing file would.
	parent := t.TempDir()
	podCgroup := filepath.Join(parent, "kubepods-besteffort-podabc.slice")
	target := filepath.Join(podCgroup, "dmem.max")

	done := make(chan error, 1)
	go func() {
		done <- SetMaxWithRetry(podCgroup, "drm/0000:06:00.0/vram", 2147483648)
	}()

	// Simulate kubelet creating the cgroup a moment after Allocate starts.
	time.Sleep(120 * time.Millisecond)
	if err := os.Mkdir(podCgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil {
		t.Fatalf("SetMaxWithRetry: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := "drm/0000:06:00.0/vram 2147483648\n"; string(got) != want {
		t.Errorf("dmem.max content = %q, want %q", got, want)
	}
}

func TestSetMaxWithRetryGivesUpEventually(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping the real ~20s retry budget")
	}
	start := time.Now()
	err := SetMaxWithRetry(filepath.Join(t.TempDir(), "never-created"), "drm/0000:06:00.0/vram", 1)
	if err == nil {
		t.Fatal("want an error when the pod cgroup never appears")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("SetMaxWithRetry took %v, want it to give up well under 25s", elapsed)
	}
}

func TestRegionIgnoresUnrelatedLines(t *testing.T) {
	root := t.TempDir()
	content := strings.Join([]string{
		"drm/0000:05:00.0/vram 17179869184",
		"drm/0000:06:00.0/vram 17095983104",
		"drm/0000:07:00.0/vram 17179869184",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "dmem.capacity"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if region, ok := Region(root, "0000:06:00.0"); !ok || region != "drm/0000:06:00.0/vram" {
		t.Errorf("Region = %q, %v; want the matching line among several", region, ok)
	}
}

// The cap is on by default, so a node without the systemd pod slices must
// report unusable instead of retrying every slice's cgroup.
func TestUsable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory dmem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Usable(root) {
		t.Error("dmem without kubepods.slice (cgroupfs driver) should be unusable")
	}
	if err := os.Mkdir(filepath.Join(root, "kubepods.slice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !Usable(root) {
		t.Error("dmem with kubepods.slice should be usable")
	}
	noDmem := t.TempDir()
	if err := os.Mkdir(filepath.Join(noDmem, "kubepods.slice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Usable(noDmem) {
		t.Error("kubepods.slice without the dmem controller should be unusable")
	}
}
