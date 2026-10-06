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
// limit (ROCm enforces the CU slice via HSA_CU_MASK, which the hook pins)
// through the LD_AUDIT interface, which musl's dynamic
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
	"regexp"
	"slices"
	"strconv"
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
	// OldGlibc is a glibc dynamic loader present, but a libc.so.6 older
	// than minGlibcMinor: loading libamvgpu's LD_AUDIT hook fails with a
	// dynamic-linker "GLIBC_2.34 not found" error instead of running
	// unprotected (Project-HAMi/HAMi#2265), but it is still a failure
	// Allocate should catch and fail closed on rather than let kubelet
	// hit it as an opaque container crash.
	OldGlibc
)

func (l Libc) String() string {
	switch l {
	case Glibc:
		return "glibc"
	case Musl:
		return "musl"
	case OldGlibc:
		return fmt.Sprintf("glibc older than %s", minGlibcVersion)
	default:
		return "unknown/static"
	}
}

// AuditCompatible reports whether libamvgpu's LD_AUDIT enforcement applies
// at all for this libc. Only a glibc new enough to provide the symbol
// versions libamvgpu itself requires dynamic-links against a loader that
// actually loads the hook.
func (l Libc) AuditCompatible() bool {
	return l == Glibc
}

// muslMarkerPrefix and glibcMarkers identify dynamic loader filenames by
// basename, so finding one does not depend on which directory a given
// distro installs it under (differs between Debian/Ubuntu, Alpine, Fedora,
// and 32-bit multiarch layouts).
const muslMarkerPrefix = "ld-musl-"

var glibcMarkers = []string{
	"ld-linux-x86-64.so.2",
	"ld-linux-aarch64.so.1",
	"ld-linux.so.2",
	"ld-linux-armhf.so.3",
}

// minGlibcMinor is libamvgpu's own minimum (see amd-hami-core
// src/include/glibc_compat.h and test/check_glibc_abi.sh): its LD_AUDIT
// hook requires GLIBC_2.34 symbol versions to load at all.
const minGlibcMinor = 34

const minGlibcVersion = "2.34"

// glibcVersionPattern matches the GLIBC_2.NN version-node strings glibc
// embeds in libc.so.6 for every version it was built to define; the
// highest one present is that libc's own version, the same fact "strings
// libc.so.6 | grep GLIBC_" relies on. A plain byte-level regex over the
// whole file avoids needing objdump/readelf installed in the (minimal,
// ROCm-runtime-based) device plugin image.
var glibcVersionPattern = regexp.MustCompile(`GLIBC_2\.([0-9]+)`)

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
	defer func() { _ = os.Remove(mountDir) }()

	imageRef = normalizeRef(imageRef)
	// "--" ends flag parsing, so a ref starting with '-' is never a flag.
	mountArgs := []string{"--namespace", ins.Namespace, "--address", ins.ContainerdSocket, "images", "mount", "--", imageRef, mountDir}
	if out, err := exec.CommandContext(ctx, ins.CtrPath, mountArgs...).CombinedOutput(); err != nil {
		return Unknown, fmt.Errorf("ctr images mount %s: %w (%s)", imageRef, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		unmountArgs := []string{"--namespace", ins.Namespace, "--address", ins.ContainerdSocket, "images", "unmount", mountDir}
		// Best effort: a failed unmount here leaks a mount, not a
		// correctness problem for the caller's fail-closed decision.
		_ = exec.CommandContext(context.Background(), ins.CtrPath, unmountArgs...).Run()
	}()

	libc, err := findLibc(mountDir)
	if err != nil || libc != Glibc {
		return libc, err
	}
	okVersion, err := glibcAtLeastMinimum(mountDir)
	if err != nil {
		return Unknown, fmt.Errorf("check glibc version: %w", err)
	}
	if !okVersion {
		return OldGlibc, nil
	}
	return Glibc, nil
}

// normalizeRef expands a pod spec's unqualified image reference (e.g.
// "alpine:3.20") to the fully qualified form containerd actually stores it
// under (e.g. "docker.io/library/alpine:3.20"), matching Docker Hub's own
// implicit-registry rule. Found via a real Allocate failure on Claven: "ctr
// images mount alpine:3.20" reported "not found" even though kubelet had
// already pulled the image, because containerd indexes it under the
// qualified name.
func normalizeRef(ref string) string {
	domain, remainder, hasSlash := strings.Cut(ref, "/")
	if !hasSlash || (!strings.ContainsAny(domain, ".:") && domain != "localhost") {
		remainder = ref
		domain = "docker.io"
		if !hasSlash {
			remainder = "library/" + ref
		}
	}
	if !strings.Contains(remainder[strings.LastIndex(remainder, "/")+1:], ":") && !strings.Contains(remainder, "@") {
		remainder += ":latest"
	}
	return domain + "/" + remainder
}

// glibcAtLeastMinimum reports whether root's libc.so.6 defines
// GLIBC_2.minGlibcMinor or newer. Only called once findLibc has already
// found a glibc dynamic loader, so a missing libc.so.6 is itself an
// error, not just "unknown". A libc.so.6 in a standard library directory
// wins over one vendored elsewhere (e.g. under /opt): it is the one the
// standard loader resolves.
func glibcAtLeastMinimum(root string) (bool, error) {
	r, err := scan(root)
	if err != nil {
		return false, fmt.Errorf("walk mounted image for libc.so.6: %w", err)
	}
	libcPath := r.libcStd
	if libcPath == "" {
		libcPath = r.libcOther
	}
	if libcPath == "" {
		return false, fmt.Errorf("libc.so.6 not found despite a glibc dynamic loader")
	}
	data, err := os.ReadFile(libcPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", libcPath, err)
	}
	maxMinor := -1
	for _, m := range glibcVersionPattern.FindAllSubmatch(data, -1) {
		if minor, err := strconv.Atoi(string(m[1])); err == nil && minor > maxMinor {
			maxMinor = minor
		}
	}
	if maxMinor < 0 {
		return false, fmt.Errorf("no GLIBC_2.x version strings found in %s", libcPath)
	}
	return maxMinor >= minGlibcMinor, nil
}

// findLibc classifies root by the dynamic loader its binaries would use. A
// glibc loader in a standard library directory wins over a musl loader, so a
// glibc image that also ships Debian's musl package is still glibc; a musl
// loader wins over a glibc loader vendored elsewhere, so an Alpine image
// carrying a glibc copy under /opt is still musl.
func findLibc(root string) (Libc, error) {
	r, err := scan(root)
	if err != nil {
		return Unknown, fmt.Errorf("walk mounted image: %w", err)
	}
	switch {
	case r.glibcStd:
		return Glibc, nil
	case r.musl:
		return Musl, nil
	case r.glibcOther:
		return Glibc, nil
	}
	return Unknown, nil
}

// stdLibDir matches the image-relative directories a distro installs its
// glibc loader and libc.so.6 in (Debian/Ubuntu multiarch, Fedora/RHEL lib64,
// lib32), as opposed to a copy vendored under /opt or an application dir.
var stdLibDir = regexp.MustCompile(`^(usr/)?lib(32|64)?(/[^/]+-linux-gnu[^/]*)?$`)

type scanResult struct {
	glibcStd, glibcOther, musl bool
	libcStd, libcOther         string // first libc.so.6 found in each class
}

// scan walks root once, without following symlinks (an absolute symlink in
// a mounted image would resolve against the host), and records which
// dynamic loaders and libc.so.6 copies the image contains.
func scan(root string) (scanResult, error) {
	var r scanResult
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil && path == root {
			return err
		}
		// Any other error is one unreadable entry inside the image:
		// skipping it and scanning the rest is deliberate.
		if err == nil && !d.IsDir() {
			r.record(root, path, d.Name())
		}
		return nil
	})
	return r, err
}

func (r *scanResult) record(root, path, name string) {
	rel, _ := filepath.Rel(root, filepath.Dir(path))
	std := stdLibDir.MatchString(filepath.ToSlash(rel))
	switch {
	case slices.Contains(glibcMarkers, name):
		r.glibcStd = r.glibcStd || std
		r.glibcOther = r.glibcOther || !std
	case strings.HasPrefix(name, muslMarkerPrefix):
		r.musl = true
	case name == "libc.so.6" && std && r.libcStd == "":
		r.libcStd = path
	case name == "libc.so.6" && !std && r.libcOther == "":
		r.libcOther = path
	}
}
