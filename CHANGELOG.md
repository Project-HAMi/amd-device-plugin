# Changelog

## v0.0.3

### Fixed

- The bundled `libamvgpu.so` publishes its shared region and pod environment to other threads with acquire/release, so a thread can no longer see them ready before they are; ThreadSanitizer is now part of the hook's CI (#143, Project-HAMi/amd-hami-core#20).

## v0.0.2

### Added

- Multi-GPU pods: `ROCR_VISIBLE_DEVICES`, `HSA_CU_MASK` and `HIP_DEVICE_MEMORY_LIMIT_<i>` follow one container-local order, with a regression test (#100); per-GPU memory limits (#103) and dmem caps (#106, #108).
- APUs and iGPUs without a ROCr UUID are registered and addressed by container-local index (#101).
- RDNA CU slices are whole WGPs, and `custominfo.cuPerWGP` lets the scheduler account the same count (#105, #107).
- `-split_count` defaults to 2 sharers per GPU on gfx12, 10 elsewhere (#90, #109); compute-queue slots are published in `custominfo.computeQueues` (#88).
- Kernel dmem VRAM cap, on by default where the node supports it (#99, #110).
- Opt-in fail-closed check for images that cannot load the memory hook (#104, #106).
- CDI injection (#91), `spread` and `binpack` allocator policies (#85, #96), per-GPU health from the DRM device (#87).
- The bundled `libamvgpu.so` pins `HSA_CU_MASK` to the pod spec (#111, Project-HAMi/amd-hami-core#13).
- SECURITY.md, SECURITY-INSIGHTS.yml and CONTRIBUTING.md (#46); OpenSSF Scorecard (#47), CodeQL (#49), SHA-pinned actions with build provenance (#48), digest-pinned base images and a CU-list fuzz test (#114).

### Fixed

- The allocator works on nodes whose GPUs have no direct peer links (#102) and on single GPUs split into slots (#92).
- Whole-GPU requests from the HAMi scheduler are recognized (#95); RDNA4 GPUs are recognized as a family (#97).
- The labeller and UBI images build again (#94).
- Helm chart releases skip an already released chart version instead of failing (#115).
- besteffort allocator no longer fails to initialize on single-GPU nodes (#56).
- Missing CDNA partition sysfs files are no longer reported as warnings on RDNA (#57).
- Whole-GPU requests no longer inject the `libamvgpu.so` `LD_AUDIT` hook or `HIP_DEVICE_MEMORY_LIMIT`, so musl and older glibc images start (#58).
- Node labeller compares the DRM render minor without narrowing int64 (#61).
- `labeller.Dockerfile` builds with Go 1.26 to satisfy `go.mod` (#80).
- The memory hook counts concurrent allocations exactly, so parallel `hipMalloc` calls cannot overshoot the limit (#126, Project-HAMi/amd-hami-core#14).
- Memory-only slices hold no CUs, plugin restarts end stale kubelet streams, and GPU discovery data is read under a lock (#125).
- Split devices work with the `besteffort` and `spread` allocators (#120); node locks are always released (#118); per-GPU discovery state resets between scans and MI300 partitions take their parent GPU's health (#123).
- The musl check picks the loader the image actually uses, and CU bitmaps stay intact on allocation errors (#121).
- The node labeller, chart RBAC and image CI are hardened (#119).
- An unreachable metrics exporter is logged once per outage instead of every health check (#127, #140).
- Allocate no longer panics on an annotation that names more containers than the pod has; the musl check reads the image's own libc through absolute symlinks and walks the image once; a retried plugin Start no longer leaks a refresh loop (#130).
- The node labeller exits when `DS_NODE_NAME` is unset instead of labelling nothing, and RDNA3/RDNA3.5 GPUs get a family label with older libdrm headers (#129).
- The JAX examples run, the TensorFlow example no longer restarts forever, and the vLLM example starts without the optional token Secret (#139, #141).

### Changed

- k8s.io modules are at v0.37.1, including `k8s.io/kubelet` (#60, #78); protobuf uses the released v1.36.12.
- The hook is built from the `amd-hami-core` submodule; the checked-in `libamvgpu.so` binary is removed (#81). hami-core builds on ROCm 7.2.4 in all Dockerfiles (#80).
- Replaced deprecated `io/ioutil` and `grpc.Dial`, and fixed staticcheck findings (#59, #66, #75).
- Docs build dependencies are bumped to patched versions (#40, #79 and Dependabot updates); grpc is past GO-2026-6443 (#113).
- The chart icon is the HAMi logo instead of the bundled AMD `logo.png` (#112).
- README, user guide and repository files are rewritten for the HAMi plugin (#116, #117, #122); remaining lint findings are fixed (#124).
- The UBI images verify the Go toolchain checksum (#128).
- The chart labels the DaemonSet, takes the log level from `dp.logVerbosity` (default 2 instead of a fixed `-v=5`) and uses the chart `appVersion` as the default image tag (#131).
- CI runs `go vet` and the race detector and renders the optional chart branches; Dependabot proposes `amd-hami-core` bumps (#134).
- `amd-hami-core` is bumped for its CI hardening and an initialization guard that cannot re-enter the dynamic linker (#133).
- The raw `k8s-ds-amdgpu-dp*.yaml` manifests are removed: they lacked the RBAC HAMi needs; use the Helm chart.

## v0.0.1

Initial Project-HAMi release of the AMD device plugin.

- Reports AMD SMI UUIDs and product names in HAMi device annotations.
- Reports PCI BDF through `custominfo.pciBDF` and physical VRAM/CU capacity through `libdrm_amdgpu`.
- Applies ROCr device visibility, CU masks, and fractional-memory limits during allocation.
- Persists per-Pod CU allocation state across device-plugin restarts.
- Packages the temporary `libamvgpu.so` hook and installs it on each node with a HAMi-style `postStart` lifecycle hook.
- Ships Helm chart `0.0.1` with the required ServiceAccount, RBAC, host hook delivery, and AMD device access configuration.

Known limitation: the bundled memory hook requires glibc symbols through `GLIBC_2.34`; see the README for workload-image compatibility details.
