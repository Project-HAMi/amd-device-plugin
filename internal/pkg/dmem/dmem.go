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

// Package dmem writes VRAM caps to the kernel dmem cgroup v2 controller, a
// hard, driver-level backend that needs no LD_AUDIT injection and no glibc
// version (see Project-HAMi/amd-hami-core#6). It complements, and does not
// replace, libamvgpu: dmem caps real allocations at the driver, but
// hipMemGetInfo still reports the whole card inside a capped cgroup, so
// engines still need libamvgpu's override to size themselves correctly.
//
// Known gaps, measured on RX 9060 XT (gfx1200), kernel 7.0.0, cgroup v2:
//   - hipMallocAsync pools bypass the VRAM cap and spill into GTT (system
//     RAM), which no cgroup charges.
//   - Lowering dmem.max below current usage does not reclaim on this
//     kernel; the cap must be set before the workload allocates.
//   - A whole-GPU pod on the same card as sliced pods gets no cap
//     (dmem.max stays "max") and can consume VRAM budgeted to the slices;
//     the scheduler must not co-locate them (Project-HAMi/HAMi#3144).
package dmem

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// DefaultCgroupRoot is the standard cgroup v2 unified hierarchy mount point.
const DefaultCgroupRoot = "/sys/fs/cgroup"

// NormalizeBDF converts the KFD topology's four-colon BDF spelling
// (domain:bus:device:function, e.g. "0000:06:00:0") to the standard PCI
// spelling dmem.capacity uses (domain:bus:device.function, e.g.
// "0000:06:00.0"), by replacing the last colon with a dot. Already
// dot-spelled BDFs pass through unchanged.
func NormalizeBDF(bdf string) string {
	if strings.Contains(bdf, ".") {
		return bdf
	}
	i := strings.LastIndex(bdf, ":")
	if i < 0 {
		return bdf
	}
	return bdf[:i] + "." + bdf[i+1:]
}

// Available reports whether the dmem controller is active in this cgroup
// hierarchy (listed in the root cgroup.controllers).
func Available(cgroupRoot string) bool {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return false
	}
	for _, c := range strings.Fields(string(b)) {
		if c == "dmem" {
			return true
		}
	}
	return false
}

// Usable reports whether pods on this node can be capped: the dmem controller
// is active and the systemd cgroup driver's kubepods.slice exists, which is
// the only layout PodCgroupPath knows.
func Usable(cgroupRoot string) bool {
	if !Available(cgroupRoot) {
		return false
	}
	fi, err := os.Stat(filepath.Join(cgroupRoot, "kubepods.slice"))
	return err == nil && fi.IsDir()
}

// Region returns the dmem region name for bdf (e.g. "drm/0000:06:00.0/vram")
// and whether the GPU driver registered a VRAM region for it at all. GPUs
// without driver-level VRAM accounting (or BDFs that do not exist) report
// false; the caller must fall back to the libamvgpu-only path.
func Region(cgroupRoot, bdf string) (string, bool) {
	f, err := os.Open(filepath.Join(cgroupRoot, "dmem.capacity"))
	if err != nil {
		return "", false
	}
	defer f.Close()

	want := "drm/" + strings.ToLower(bdf) + "/vram"
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 1 && strings.ToLower(fields[0]) == want {
			return fields[0], true
		}
	}
	return "", false
}

// PodCgroupPath returns the systemd cgroup driver's v2 path for a pod, given
// its UID and QoS class. Matches kubelet's naming convention:
// kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice, with
// dashes in the UID replaced by underscores; Guaranteed pods omit the QoS
// segment. Verified against a running RKE2 node (systemd driver, cgroup v2).
func PodCgroupPath(cgroupRoot string, podUID string, qos corev1.PodQOSClass) string {
	uidSlug := strings.ReplaceAll(podUID, "-", "_")
	switch qos {
	case corev1.PodQOSBurstable:
		return filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-burstable.slice",
			fmt.Sprintf("kubepods-burstable-pod%s.slice", uidSlug))
	case corev1.PodQOSBestEffort:
		return filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
			fmt.Sprintf("kubepods-besteffort-pod%s.slice", uidSlug))
	default: // corev1.PodQOSGuaranteed
		return filepath.Join(cgroupRoot, "kubepods.slice",
			fmt.Sprintf("kubepods-pod%s.slice", uidSlug))
	}
}

// SetMax caps region at limitBytes on the pod cgroup at podCgroupPath. The
// path must already exist; the caller decides whether a missing pod cgroup
// (sandbox not yet created, or a non-systemd cgroup driver) is fatal or a
// silent fallback to libamvgpu-only enforcement.
func SetMax(podCgroupPath, region string, limitBytes int64) error {
	target := filepath.Join(podCgroupPath, "dmem.max")
	line := fmt.Sprintf("%s %d\n", region, limitBytes)
	if err := os.WriteFile(target, []byte(line), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// SetMaxWithRetry calls SetMax, retrying while the pod cgroup does not exist
// yet. The caller is expected to run this off the Allocate response path (in
// its own goroutine): measured on a real RKE2 node (systemd driver), the
// gap between kubelet binding the pod and its cgroup appearing varied from
// under a second up to ~7s, so this budgets a generous ~20s total before
// giving up on a persistently missing cgroup (non-systemd driver, or one
// that never appears).
func SetMaxWithRetry(podCgroupPath, region string, limitBytes int64) error {
	delays := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1500 * time.Millisecond, 3000 * time.Millisecond,
		5000 * time.Millisecond, 9000 * time.Millisecond,
	}
	var err error
	for _, d := range delays {
		err = SetMax(podCgroupPath, region, limitBytes)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(d)
	}
	return SetMax(podCgroupPath, region, limitBytes)
}
