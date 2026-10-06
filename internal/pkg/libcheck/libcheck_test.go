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

package libcheck

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindLibcMusl(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "lib", "ld-musl-x86_64.so.1"))
	touch(t, filepath.Join(root, "lib", "libc.musl-x86_64.so.1"))

	got, err := findLibc(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != Musl {
		t.Errorf("findLibc = %v, want Musl", got)
	}
	if got.AuditCompatible() {
		t.Error("musl must not be reported LD_AUDIT-compatible")
	}
}

func TestFindLibcGlibcDebianLayout(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "lib", "x86_64-linux-gnu", "ld-linux-x86-64.so.2"))

	got, err := findLibc(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != Glibc {
		t.Errorf("findLibc = %v, want Glibc", got)
	}
	if !got.AuditCompatible() {
		t.Error("glibc should be reported LD_AUDIT-compatible")
	}
}

func TestFindLibcGlibcFedoraLayout(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "usr", "lib64", "ld-linux-x86-64.so.2"))

	got, err := findLibc(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != Glibc {
		t.Errorf("findLibc = %v, want Glibc", got)
	}
}

func TestFindLibcStaticOrScratch(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "app"))

	got, err := findLibc(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != Unknown {
		t.Errorf("findLibc = %v, want Unknown (static/scratch)", got)
	}
	if got.AuditCompatible() {
		t.Error("Unknown must not be reported LD_AUDIT-compatible")
	}
}

func TestFindLibcEmptyRoot(t *testing.T) {
	got, err := findLibc(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != Unknown {
		t.Errorf("findLibc(empty) = %v, want Unknown", got)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"alpine:3.20":          "docker.io/library/alpine:3.20",
		"alpine":               "docker.io/library/alpine:latest",
		"someuser/myimage:tag": "docker.io/someuser/myimage:tag",
		"ghcr.io/project-hami/amd-device-plugin:0.0.1": "ghcr.io/project-hami/amd-device-plugin:0.0.1",
		"myregistry.io/foo":                            "myregistry.io/foo:latest",
		"localhost:5000/foo":                           "localhost:5000/foo:latest",
		"localhost/foo:bar":                            "localhost/foo:bar",
		"alpine@sha256:deadbeef":                       "docker.io/library/alpine@sha256:deadbeef",
	}
	for in, want := range cases {
		if got := normalizeRef(in); got != want {
			t.Errorf("normalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}
