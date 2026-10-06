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
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestFindLibcPrefersStandardGlibcLoader(t *testing.T) {
	for name, tc := range map[string]struct {
		files []string
		want  Libc
	}{
		"glibc image with Debian musl package": {
			[]string{"usr/lib/musl/lib/ld-musl-x86_64.so.1", "usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2"}, Glibc},
		"musl image with glibc vendored under opt": {
			[]string{"lib/ld-musl-x86_64.so.1", "opt/glibc/lib/ld-linux-x86-64.so.2"}, Musl},
	} {
		root := t.TempDir()
		for _, f := range tc.files {
			touch(t, filepath.Join(root, f))
		}
		if got, err := findLibc(root); err != nil || got != tc.want {
			t.Errorf("%s: findLibc = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

func TestFindLibcUnreadableRoot(t *testing.T) {
	if _, err := findLibc(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("findLibc on a missing root must error, not report Unknown")
	}
}

func TestGlibcAtLeastMinimumPrefersStandardLibc(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string]string{
		"opt/vendor/lib/libc.so.6":           "GLIBC_2.17\x00",
		"usr/lib/x86_64-linux-gnu/libc.so.6": "GLIBC_2.38\x00",
	} {
		touch(t, filepath.Join(root, path))
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := glibcAtLeastMinimum(root); err != nil || !ok {
		t.Fatalf("glibcAtLeastMinimum = %v, %v; want the /usr libc (2.38), not the vendored /opt one", ok, err)
	}
}

func TestInspectEndsFlagsBeforeImageRef(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	ctr := filepath.Join(dir, "ctr")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit 1\n"
	if err := os.WriteFile(ctr, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ins := &Inspector{CtrPath: ctr, ContainerdSocket: "/sock", Namespace: "k8s.io"}
	if _, err := ins.Inspect(context.Background(), "alpine"); err == nil {
		t.Fatal("expected the fake ctr's failure")
	}
	out, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "mount\n--\ndocker.io/library/alpine:latest\n") {
		t.Fatalf("ctr args = %q, want \"--\" right before the image ref", out)
	}
}

func TestGlibcAtLeastMinimum(t *testing.T) {
	writeLibc := func(t *testing.T, versions ...string) string {
		t.Helper()
		root := t.TempDir()
		path := filepath.Join(root, "lib", "x86_64-linux-gnu", "libc.so.6")
		touch(t, path)
		data := "\x00binary noise\x00"
		for _, v := range versions {
			data += "GLIBC_" + v + "\x00"
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("new enough", func(t *testing.T) {
		root := writeLibc(t, "2.2.5", "2.17", "2.34", "2.38")
		ok, err := glibcAtLeastMinimum(root)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("glibcAtLeastMinimum = false, want true for a libc defining up to 2.38")
		}
	})

	t.Run("too old", func(t *testing.T) {
		root := writeLibc(t, "2.2.5", "2.17", "2.31")
		ok, err := glibcAtLeastMinimum(root)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("glibcAtLeastMinimum = true, want false for a libc defining only up to 2.31")
		}
	})

	t.Run("missing libc.so.6", func(t *testing.T) {
		if _, err := glibcAtLeastMinimum(t.TempDir()); err == nil {
			t.Error("expected an error when libc.so.6 is absent")
		}
	})
}

func TestFindLibcAndVersionCheckCompose(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "lib", "x86_64-linux-gnu", "ld-linux-x86-64.so.2"))
	path := filepath.Join(root, "lib", "x86_64-linux-gnu", "libc.so.6")
	if err := os.WriteFile(path, []byte("GLIBC_2.31\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	libc, err := findLibc(root)
	if err != nil {
		t.Fatal(err)
	}
	if libc != Glibc {
		t.Fatalf("findLibc = %v, want Glibc (version check is separate)", libc)
	}
	ok, err := glibcAtLeastMinimum(root)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected glibc 2.31 to be reported as too old")
	}
}

func TestOldGlibcIsNotAuditCompatible(t *testing.T) {
	if OldGlibc.AuditCompatible() {
		t.Error("OldGlibc must not be reported LD_AUDIT-compatible")
	}
	if OldGlibc.String() == "" {
		t.Error("OldGlibc.String() must not be empty")
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
