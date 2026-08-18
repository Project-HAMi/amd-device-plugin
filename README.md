# HAMi AMD Device Plugin

[![CI](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/Project-HAMi/amd-device-plugin)](LICENSE)

This repository contains the AMD device plugin used by [HAMi](https://github.com/Project-HAMi/HAMi) to discover AMD GPUs and enforce HAMi vGPU allocations on Kubernetes nodes. It is based on AMD's upstream [ROCm Kubernetes device plugin](https://github.com/ROCm/k8s-device-plugin).

## Current capabilities

- Uses the AMD SMI C API through cgo to query device UUIDs and product names.
- Publishes the hardware-bound AMD SMI UUID as `DeviceInfo.ID`.
- Publishes the AMD SMI ASIC market name, for example `AMD Instinct MI300X VF`, as `DeviceInfo.Type`.
- Publishes the standard PCI BDF in `custominfo.pciBDF`.
- Reads physical VRAM and active CU capacity through `libdrm_amdgpu`.
- Publishes the user compute queue (HQD) slots KFD reports (`num_cp_queues`) in `custominfo.computeQueues`.
- Persists per-Pod CU ranges in `hami.io/amd-cu-allocated` and reconstructs allocation state after a device-plugin restart.
- Optionally injects GPUs through CDI (`--cdi_spec_dir`, Helm `dp.cdi.enabled`): it writes an `amd.com/gpu` spec with one device per DRM card plus `/dev/kfd` and returns CDI device names from Allocate.
- Supports the kubelet preferred-allocation policies `besteffort` (default), `binpack` (closest devices) and `spread` (farthest devices) through `--allocator_policy` (Helm `dp.allocatorPolicy`).
- Applies `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, and `HSA_CU_MASK` in the same container-local device order for multi-GPU allocations.
- Applies the requested memory limit through `HIP_DEVICE_MEMORY_LIMIT` and the `libamvgpu.so` `LD_AUDIT` hook for core or memory slices. Whole-GPU requests do not use the hook.

The plugin registers devices in the `hami.io/node-amd-register` node annotation. A device entry has this shape:

```json
{
  "id": "8eff74b5-0000-1000-801b-b56457addd1b",
  "index": 0,
  "count": 10,
  "devmem": 196288,
  "devcore": 304,
  "type": "AMD Instinct MI300X VF",
  "numa": 0,
  "health": true,
  "devicevendor": "amd",
  "custominfo": {
    "pciBDF": "0000:83:00.0"
  }
}
```

## Requirements

- Linux `amd64` AMD GPU node supported by ROCm.
- Kubernetes and a compatible HAMi scheduler deployment.
- AMD GPU kernel driver, `/dev/kfd`, `/dev/dri`, KFD topology under `/sys`, and `libdrm_amdgpu`.
- AMD SMI from ROCm 7.2.4. The image carries the matching AMD SMI userspace library; the host must provide the compatible kernel driver and device interfaces.
- Permission for the DaemonSet service account to read Pods and patch Node/Pod annotations and the HAMi node lock.

GPUs for which AMD SMI does not return a UUID are deliberately not registered. There is no node-name/BDF-derived compatibility ID.

## Build

```bash
docker build -t ghcr.io/project-hami/amd-device-plugin:0.0.1 .
```

The Docker build compiles the cgo code against the ROCm 7.2.4 AMD SMI SDK and packages the `libamvgpu.so` hook built from the `amd-hami-core` submodule. CI verifies that the hook exists and uses the same Dockerfile for the published image, so a missing hook fails the image build.

## Deploy with Helm

The device-plugin image includes the `libamvgpu.so` built from the `amd-hami-core` submodule under `/opt/hami/lib/amd`, separate from the host-mounted destination. Following HAMi's hook-delivery model, the device-plugin container mounts the node's `<hostHookPath>/vgpu` directory and runs `amd-vgpu-init.sh` from a `postStart` lifecycle hook. The script compares the bundled and installed files and atomically updates `<hostHookPath>/vgpu/libamvgpu.so` when needed. With the default `hostHookPath=/usr/local`, Allocate then mounts `/usr/local/vgpu/libamvgpu.so` from the host into workload containers.

Bundling the hook in the image is a temporary delivery mechanism until `amd-hami-core` provides a release and consumption pipeline. Set `dp.hookInstaller.enabled=false` only when the hook is managed on every node by another mechanism.

```bash
helm upgrade --install amd-gpu ./helm/amd-gpu \
  --namespace kube-system \
  --create-namespace
```

Chart `0.0.1` deploys image `ghcr.io/project-hami/amd-device-plugin:0.0.1` by default. Images are published to GitHub Container Registry after CI succeeds on `main` and version tags. The GHCR package must be public for deployment without credentials; otherwise configure `imagePullSecrets`.

Verify registration:

```bash
kubectl get node <node-name> -o jsonpath='{.metadata.annotations.hami\.io/node-amd-register}'
```

## Operating modes

The plugin follows HAMi's Ascend vNPU model: one plugin registration, per-pod mode
selection. Every device is registered in both forms and the scheduler picks the
form per pod based on the pod annotation `hami.io/amd-mode`:

- absent or `rocm` (default): soft mode. The device is published as a CU-maskable
  device (Count 10) and the scheduler allocates CU slices via the amd-hami-core
  hook library (`HSA_CU_MASK`, `HIP_DEVICE_MEMORY_LIMIT`, `LD_AUDIT`).
- `spx`, `cpx`, `dpx` or `qpx`: hard partition mode. The same XCP compute
  partition is published as a whole device (Count 1, ID `<rocr-uuid>#<mode>`)
  and the scheduler allocates it exclusively without a CU mask. The node must be
  switched to the matching compute partition profile first, e.g.:

  ```bash
  echo spx > /sys/class/drm/card0/device/current_compute_partition
  echo nps1 > /sys/class/drm/card0/device/current_memory_partition
  reboot   # partition changes require a reset
  ```

  Physical Instinct parts expose XCP compute partitions and serve both modes,
  discrete GPUs and AI MAX APUs (MI300A/MI350A) alike. Virtio Instinct devices
  (SR-IOV VFs) and partition-less embedded APUs register no hard entries and
  only serve rocm mode; `hami.io/amd-mode: spx` cannot be scheduled there.

The register annotation (`hami.io/node-amd-register`) carries one entry per
form: soft entries have `Mode` empty, hard entries carry `Mode` equal to the
compute partition type. The scheduler branches on `Mode` exactly like HAMi
branches on the NVIDIA `MigMode` and the Ascend `huawei.com/vnpu-mode` values.
Allocated devices must be written back as the published `DeviceInfo.ID`
(`amd-smi` UUID for whole GPUs, `GPU-<unique_id>` or `<rocr-uuid>#<mode>` for
partitions); the plugin resolves all of them.

Whole-GPU devices whose KFD `unique_id` is 0 (embedded APUs without a PCI
Device Serial Number) cannot be addressed by `GPU-<unique_id>`; ROCr addresses
them by agent index and the plugin publishes that index as the ROCr-visible
id. Such nodes expose no XCP partitions and only serve rocm mode; parts with a
PCI Device Serial Number register by `GPU-<unique_id>` instead.

### Partition profiles

An Instinct GPU can be carved into compute partitions; which partitions exist
and how they split the silicon depends on the selected profile. MI355X-class
GPUs (8 XCCs) support four profiles, reported per GPU by
`amd-smi partition -g <gpu-id> --json`:

| profile | type       | memory caps | partitions x XCC |
|---------|------------|-------------|-------------------|
| 0       | SPX (default) | NPS1     | 1 x 8             |
| 1       | DPX        | NPS1,NPS2   | 2 x 4             |
| 2       | QPX        | NPS1        | 4 x 2             |
| 3       | CPX        | NPS1        | 8 x 1             |

- `profile` is the `profile_index` passed to the AMD SMI setter
  (`amdsmi_set_gpu_accelerator_partition_profile`). The amd-smi CLI marks the
  current profile with `*` (SPX here).
- `partitions x XCC` is what the profile yields: e.g. QPX splits the GPU into
  4 partitions of 2 XCCs each. Every partition appears as an XCP device
  (`/sys/devices/platform/amdgpu_xcp_*`) and is registered by the plugin as a
  hard entry of that mode.
- `memory caps` lists the NPS modes the profile can run with (DPX also works
  under NPS2; the others are NPS1-only on this part).

Changing the profile requires the GPU to be idle (no workloads; see the
`amdsmi_set_gpu_*_partition` docs). Compute partition changes are per-GPU and
take effect live - the XCP devices appear without a reset. A memory partition
change requires an amdgpu driver reload, which resets the device nodes of
every GPU on the node: that is a node-wide quiet-window operation, not a
single-GPU one.

The plugin registers the partitions of the mode each GPU is currently in, and
advertises the available profiles per GPU (`partitionProfiles` in
DeviceInfo.CustomInfo of the register annotation) so schedulers can see which
modes a GPU can be switched to.

## Memory-isolation compatibility

CU isolation and device visibility use ROCr interfaces and are independent of the workload image's libc. Fractional-memory enforcement is different: it depends on loading `/usr/local/vgpu/libamvgpu.so` through glibc `LD_AUDIT`.

The hook built from `amd-hami-core` requires glibc symbol versions through `GLIBC_2.34`. It is therefore not compatible with older glibc images such as Ubuntu 20.04 or RHEL 8, and `LD_AUDIT` is not supported by musl/Alpine workloads. Until ABI selection and fail-closed validation are implemented, use a compatible glibc workload image for fractional-memory allocations. The hook is injected only for core or memory slices, so whole-GPU requests work on any workload image.

See [Project-HAMi/HAMi#2265](https://github.com/Project-HAMi/HAMi/issues/2265) for the compatibility discussion.

## CU isolation

The CU slice set through `HSA_CU_MASK` is a cooperative limit, not a hard guarantee. KFD creates compute queues with all CUs and the mask is applied afterwards by an ioctl the process itself controls, so a workload can drop or widen it and take the whole GPU, slowing its neighbours. Do not rely on CU slices to isolate untrusted tenants. Enforcement needs a CU ceiling in KFD; see [#55](https://github.com/Project-HAMi/amd-device-plugin/issues/55).

## Compute-queue contention

Processes sharing a GPU also share its user compute queue (HQD) slots, which `custominfo.computeQueues` reports. On gfx12 amdgpu reserves half of the slots for kernel compute rings by default, leaving 4. Two or more ROCm processes on the same GPU can then contend for dispatch and lose most of their throughput, independent of the CU and memory slices. Loading amdgpu with `num_kcq=0` frees the reserved slots; its effect depends on the ASIC and firmware, so measure it per node rather than assume it. On gfx12 nodes, consider limiting sharers per GPU with `--split_count` (Helm `dp.splitCount`), for example 2. See [#54](https://github.com/Project-HAMi/amd-device-plugin/issues/54).

## Validation status

The AMD SMI UUID, product type, BDF, VRAM and CU registration path has been validated on a real ROCm 7.0.2 `AMD Instinct MI300X VF` node. Device discovery, per-GPU health and compute-queue reporting have also been validated on an `AMD Radeon RX 9060 XT` (gfx1200, RDNA4) node. Multi-GPU `ROCR_VISIBLE_DEVICES` ordering still requires validation on nodes with more than one allocatable GPU.

## Development

Run the same checks used by CI:

```bash
docker build --target builder -t amd-device-plugin-builder:test .
docker run --rm \
  --workdir /go/src/github.com/Project-HAMi/amd-device-plugin \
  --env LD_LIBRARY_PATH=/opt/rocm/lib \
  amd-device-plugin-builder:test \
  bash -c 'ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.26 && go test ./...'

helm lint ./helm/amd-gpu
helm template amd-gpu ./helm/amd-gpu --namespace kube-system >/dev/null
```

Hardware-dependent tests skip automatically when no AMD GPU is present. Real-node validation is still required for changes to device discovery, AMD SMI calls, ROCr visibility, CU masks, or memory interception.

## License

Apache License 2.0. See [LICENSE](LICENSE).
