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
- Persists per-Pod CU ranges in `hami.io/amd-cu-allocated` and reconstructs allocation state after a device-plugin restart.
- Applies `ROCR_VISIBLE_DEVICES` and `HSA_CU_MASK` in the same container-local device order for multi-GPU allocations.
- Applies the requested memory limit through `HIP_DEVICE_MEMORY_LIMIT` and the `libamvgpu.so` `LD_AUDIT` hook.

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
- AMD SMI from ROCm 7.0.2. The image carries the matching AMD SMI userspace library; the host must provide the compatible kernel driver and device interfaces.
- Permission for the DaemonSet service account to read Pods and patch Node/Pod annotations and the HAMi node lock.

GPUs for which AMD SMI does not return a UUID are deliberately not registered. There is no node-name/BDF-derived compatibility ID.

## Build

```bash
docker build -t ghcr.io/project-hami/amd-device-plugin:0.0.1 .
```

The Docker build compiles the cgo code against the ROCm 7.0.2 AMD SMI SDK and temporarily packages the checked-in `libamvgpu.so`. CI verifies that the hook exists and uses the same Dockerfile for the published image, so a missing hook fails the image build.

## Deploy with Helm

The device-plugin image currently includes the repository's `libamvgpu.so` under `/opt/hami/lib/amd`, separate from the host-mounted destination. Following HAMi's hook-delivery model, the device-plugin container mounts the node's `<hostHookPath>/vgpu` directory and runs `amd-vgpu-init.sh` from a `postStart` lifecycle hook. The script compares the bundled and installed files and atomically updates `<hostHookPath>/vgpu/libamvgpu.so` when needed. With the default `hostHookPath=/usr/local`, Allocate then mounts `/usr/local/vgpu/libamvgpu.so` from the host into workload containers.

This bundled binary is a temporary delivery mechanism. It will be replaced by an artifact obtained from the official `amd-hami-core` repository once that project provides a release and consumption pipeline. Set `dp.hookInstaller.enabled=false` only when the hook is managed on every node by another mechanism.

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

Each XCP entry is published with an even share of the whole-GPU capacity:
VRAM and CU count divided by the number of XCP partitions of that GPU. Floor
division is used, so the advertised values under-commit rather than
over-commit. Soft entries keep the whole-GPU capacity; soft mode slices it
with CU masks.

The plugin registers the partitions of the mode each GPU is currently in, and
advertises the available profiles per GPU (`partitionProfiles` in
DeviceInfo.CustomInfo of the register annotation) so schedulers can see which
modes a GPU can be switched to.

### Profile storage

Profile data is stored in this plugin's register annotation
(`hami.io/node-amd-register`), in the same shape HAMi's NVIDIA device plugin
uses for MIG profiles (that plugin lives in the Project-HAMi/HAMi scheduler
repository, not here). It queries NVML at registration and publishes per-GPU
MIG profile capacity as `migProfiles` inside each device entry of
`hami.io/node-nvidia-register`; the scheduler reads the annotation and never
queries hardware. This plugin does the same on the AMD side: it discovers the
profiles from AMD SMI at registration and publishes them per whole GPU under
`custominfo.partitionProfiles`, one entry per profile with `profile_index`,
partition type, memory caps, partition count and XCC per partition.

Both MIG and AMD partition switching require an idle GPU. HAMi's NVIDIA
plugin start-up refuses to reset MIG-enabled GPUs that still have running
allocations, and the AMD SMI setter rejects the switch when workloads are
running. What differs is when switching happens and what it costs:

- MIG: enablement is a one-time node-level operation at plugin start-up and
  resets the GPU; afterwards instances are carved on demand from the fixed
  NVML profile menu.
- AMD: a compute partition switch is per-GPU and live - the XCP devices
  appear without a reset; a memory partition switch requires a node-wide
  amdgpu driver reload.

In both cases the stored profiles are a static menu of what the silicon can
do. The annotation carries the current mode (the device entries themselves,
with their `Mode` and `partitionProfile` fields) alongside the modes the GPU
can be switched to (`custominfo.partitionProfiles`). Allocation state stays
in the pod annotations, as XCP device IDs for hard partitions and
`hami.io/amd-cu-allocated` CU ranges in soft mode.

### Memory partitions are not an allocation mechanism

Memory partitions (NPS1/NPS2/NPS4/NPS8) are a NUMA-domain knob: they control
how the GPU's memory is interleaved across NUMA nodes at the system level,
not which pods get which bytes. They are fixed at boot (BIOS/PSP) and
changing them requires an amdgpu driver reload that resets the device nodes
of every GPU on the node.

HAMi does not read or change the memory partition. Per-pod memory limits are
enforced in software (`HIP_DEVICE_MEMORY_LIMIT` and the `libamvgpu.so`
hook), so the NPS mode has no effect on what the scheduler can allocate.
The allocation-relevant memory number is the per-XCP slice described above,
derived at plugin start from the whole-GPU capacity and the current
partition count.

Using memory partitions for scheduling would only make sense if workloads
needed OS-level NUMA locality per partition (NPS2 halves the NUMA node
size). For HAMi's slicing model that benefit is rarely worth the cost - a
boot-time setting and a node-wide driver reload to change - so the plugin
treats NPS as a fixed node property.

## Memory-isolation compatibility

CU isolation and device visibility use ROCr interfaces and are independent of the workload image's libc. Fractional-memory enforcement is different: it depends on loading `/usr/local/vgpu/libamvgpu.so` through glibc `LD_AUDIT`.

The hook currently checked into this repository requires glibc symbol versions through `GLIBC_2.34`. It is therefore not compatible with older glibc images such as Ubuntu 20.04 or RHEL 8, and `LD_AUDIT` is not supported by musl/Alpine workloads. Until ABI selection and fail-closed validation are implemented, use a compatible glibc workload image for fractional-memory allocations. The current allocation path injects the hook for every HAMi AMD allocation, including whole-GPU requests, so incompatible workload images are not yet supported safely.

See [Project-HAMi/HAMi#2265](https://github.com/Project-HAMi/HAMi/issues/2265) for the compatibility discussion.

## Validation status

The AMD SMI UUID, product type, BDF, VRAM and CU registration path has been validated on a real ROCm 7.0.2 `AMD Instinct MI300X VF` node. Multi-GPU `ROCR_VISIBLE_DEVICES` ordering still requires validation on nodes with more than one allocatable GPU.

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
