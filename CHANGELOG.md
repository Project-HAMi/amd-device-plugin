# Changelog

## Unreleased

### Fixed

- besteffort allocator no longer fails to initialize on single-GPU nodes (#56).
- Missing CDNA partition sysfs files are no longer reported as warnings on RDNA (#57).
- Whole-GPU requests no longer inject the `libamvgpu.so` `LD_AUDIT` hook or `HIP_DEVICE_MEMORY_LIMIT`, so musl and older glibc images start (#58).
- Node labeller compares the DRM render minor without narrowing int64 (#61).
- `labeller.Dockerfile` builds with Go 1.26 to satisfy `go.mod` (#80).

### Changed

- k8s.io modules are at v0.37.1, including `k8s.io/kubelet` (#60, #78); protobuf uses the released v1.36.12.
- The hook is built from the `amd-hami-core` submodule; the checked-in `libamvgpu.so` binary is removed (#81). hami-core builds on ROCm 7.2.4 in all Dockerfiles (#80).
- Replaced deprecated `io/ioutil` and `grpc.Dial`, and fixed staticcheck findings (#59, #66, #75).
- Docs build dependencies are bumped to patched versions (#79 and Dependabot updates).

## v0.0.1

Initial Project-HAMi release of the AMD device plugin.

- Reports AMD SMI UUIDs and product names in HAMi device annotations.
- Reports PCI BDF through `custominfo.pciBDF` and physical VRAM/CU capacity through `libdrm_amdgpu`.
- Applies ROCr device visibility, CU masks, and fractional-memory limits during allocation.
- Persists per-Pod CU allocation state across device-plugin restarts.
- Packages the temporary `libamvgpu.so` hook and installs it on each node with a HAMi-style `postStart` lifecycle hook.
- Ships Helm chart `0.0.1` with the required ServiceAccount, RBAC, host hook delivery, and AMD device access configuration.

Known limitation: the bundled memory hook requires glibc symbols through `GLIBC_2.34`; see the README for workload-image compatibility details.
