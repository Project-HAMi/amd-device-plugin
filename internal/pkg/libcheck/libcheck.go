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

// Package libcheck inspects a pulled-but-not-yet-running container image's
// root filesystem to tell whether it is glibc-based, musl-based (Alpine and
// similar), or statically linked. libamvgpu.so enforces the per-pod memory
// and CU-slice limits through the LD_AUDIT interface, which musl's dynamic
// linker does not implement at all and a statically linked entrypoint never
// invokes either way; both silently run unprotected instead of failing.
// amd-device-plugin's Allocate can use this to refuse a sliced allocation
// before the container ever starts (Project-HAMi/amd-hami-core#3).
//
// Mounting the image read-only (rather than guessing from its name or
// labels) requires the device plugin pod to see the containerd daemon's
// mounts: the containerd socket directory and its snapshotter data
// directory, both mounted with mountPropagation: HostToContainer. Verified
// on a real RKE2 node (Claven): without that propagation setting, "ctr
// images mount" reports success but the target directory inside the pod
// stays empty, because the daemon performs the mount in the host's own
// mount namespace and private propagation never lets it show up here.
package libcheck

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Libc identifies the C library (or absence of one) an image's entrypoint
// would load.
type Libc int

const (
	// Unknown means neither a musl nor a glibc dynamic loader was found.
	// Most likely a statically linked or "scratch" image: LD_AUDIT, which
	// only intercepts dynamic symbol binding, cannot apply to it either.
	Unknown Libc = iota
	Glibc
	Musl
)

func (l Libc) String() string {
	switch l {
	case Glibc:
		return "glibc"
	case Musl:
		return "musl"
	default:
		return "unknown/static"
	}
}

// AuditCompatible reports whether libamvgpu's LD_AUDIT enforcement applies
// at all for this libc. Only glibc dynamic-links against a loader that
// implements LD_AUDIT.
func (l Libc) AuditCompatible() bool {
	return l == Glibc
}

// muslMarkers and glibcMarkers are dynamic loader filenames, checked by
// exact basename match so this does not depend on which directory a given
// distro installs it under (differs between Debian/Ubuntu, Alpine, Fedora,
// and 32-bit multiarch layouts).
var muslMarkerPrefixes = []string{"ld-musl-"}
var glibcMarkers = []string{
	"ld-linux-x86-64.so.2",
	"ld-linux-aarch64.so.1",
	"ld-linux.so.2",
	"ld-linux-armhf.so.3",
}

// Inspector mounts images read-only via the host's containerd (through
// "ctr images mount") to determine their libc. CtrPath and
// ContainerdSocket must point at a containerd the caller's own mount
// namespace can see propagated mounts from; see the package doc.
type Inspector struct {
	CtrPath          string
	ContainerdSocket string
	Namespace        string // containerd namespace images were pulled into, usually "k8s.io"
}

// Inspect mounts imageRef read-only, inspects it for a musl or glibc
// dynamic loader, and unmounts it again. The image must already be present
// in the inspector's containerd (kubelet pulls it before Allocate runs in
// the normal flow); a missing image is reported as an error, not Unknown,
// so the caller can decide how to fail closed on "could not verify".
func (ins *Inspector) Inspect(ctx context.Context, imageRef string) (Libc, error) {
	mountDir, err := os.MkdirTemp("", "amd-dp-libcheck-")
	if err != nil {
		return Unknown, fmt.Errorf("create scratch mount dir: %w", err)
	}
	defer os.Remove(mountDir)

	mountArgs := []string{"--namespace", ins.Namespace, "--address", ins.ContainerdSocket, "images", "mount", imageRef, mountDir}
	if out, err := exec.CommandContext(ctx, ins.CtrPath, mountArgs...).CombinedOutput(); err != nil {
		return Unknown, fmt.Errorf("ctr images mount %s: %w (%s)", imageRef, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		unmountArgs := []string{"--namespace", ins.Namespace, "--address", ins.ContainerdSocket, "images", "unmount", mountDir}
		// Best effort: a failed unmount here leaks a mount, not a
		// correctness problem for the caller's fail-closed decision.
		_ = exec.CommandContext(context.Background(), ins.CtrPath, unmountArgs...).Run()
	}()

	return findLibc(mountDir)
}

func findLibc(root string) (Libc, error) {
	found := Unknown
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // permission-denied etc. on one entry: keep looking
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		for _, m := range glibcMarkers {
			if name == m {
				found = Glibc
				return fs.SkipAll
			}
		}
		for _, p := range muslMarkerPrefixes {
			if strings.HasPrefix(name, p) {
				found = Musl
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return Unknown, fmt.Errorf("walk mounted image: %w", err)
	}
	return found, nil
}
