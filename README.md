# HAMi AMD Device Plugin

[![CI](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/Project-HAMi/amd-device-plugin/badge)](https://securityscorecards.dev/viewer/?uri=github.com/Project-HAMi/amd-device-plugin)
[![License](https://img.shields.io/github/license/Project-HAMi/amd-device-plugin)](LICENSE)

The Kubernetes device plugin that lets [HAMi](https://github.com/Project-HAMi/HAMi) share AMD GPUs between pods. It registers AMD GPUs with HAMi and, when a pod starts, limits it to the compute units (CUs) and VRAM HAMi gave it. It is based on AMD's upstream [ROCm Kubernetes device plugin](https://github.com/ROCm/k8s-device-plugin).

## What it does

- Registers every AMD GPU in the `hami.io/node-amd-register` node annotation: AMD SMI UUID, product name, VRAM, CU count, PCI BDF and NUMA node.
- Gives each pod only its share of a GPU:
  - **Compute:** a CU slice through `HSA_CU_MASK`. On RDNA (gfx10 and later) slices are whole WGPs (pairs of CUs), because the GPU ignores a mask that splits a WGP.
  - **Memory:** a VRAM limit per GPU through the `libamvgpu.so` hook from [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core), and, where the node supports it, a hard kernel cap through the dmem cgroup controller.
- Handles multi-GPU pods. Device order, CU masks and memory limits stay aligned per GPU, including APUs and iGPUs without a ROCr UUID.
- Keeps CU allocations across plugin restarts, and marks a GPU unhealthy when its device node can no longer be opened.

Whole-GPU requests get no memory hook and no CU restriction, and work with any container image.

## Requirements

- Linux amd64 nodes with ROCm-supported AMD GPUs, the `amdgpu` kernel driver, `/dev/kfd` and `/dev/dri`.
- Kubernetes with the [HAMi scheduler](https://github.com/Project-HAMi/HAMi) installed. HAMi has AMD support built in.
- For memory slices, a glibc 2.34 or newer workload image (for example Ubuntu 22.04+ or RHEL 9). See [Limits](#limits).
- Optional: cgroup v2 with the systemd cgroup driver and a kernel with the dmem controller, for the hard VRAM cap.

## Install

```bash
helm upgrade --install amd-gpu ./helm/amd-gpu --namespace kube-system --create-namespace
```

Check that the node registered its GPUs:

```bash
kubectl get node <node> -o jsonpath='{.status.allocatable.amd\.com/gpu}'
kubectl get node <node> -o jsonpath='{.metadata.annotations.hami\.io/node-amd-register}'
```

## Use

Request GPUs with HAMi's AMD resources:

| Resource | Meaning |
|---|---|
| `amd.com/gpu` | Number of GPUs |
| `amd.com/gpucores` | Percentage of each GPU's CUs (1-100). Omit it for whole GPUs. |
| `amd.com/gpumem` | VRAM per GPU in MiB. Omit it for the whole VRAM. |

A quarter of one GPU with 4 GiB of VRAM:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-slice
spec:
  containers:
  - name: app
    image: rocm/pytorch:latest
    command: ["python3", "-c", "import torch; print(torch.cuda.get_device_name(0))"]
    resources:
      limits:
        amd.com/gpu: 1
        amd.com/gpucores: 25
        amd.com/gpumem: 4096
```

More examples are in [example/](example/).

## Operating modes

The mode is chosen per node (`OPERATING_MODE`, or the
`hami.io/amd-operating-mode` node annotation), like NVIDIA's `mig.config`:

- `cu` (default): every whole GPU is published as a CU-maskable device (Count
  `splitCount`) and the scheduler allocates CU slices via the amd-hami-core hook
  library (`HSA_CU_MASK`, `HIP_DEVICE_MEMORY_LIMIT`, `LD_AUDIT`).
- `partition`: every compute partition is published as a whole device (Count 1,
  ID suffixed `#<type>`) and allocated exclusively without a CU mask. XCP
  partitions replace their parent GPU. With `COMPUTE_PARTITION` set, the plugin
  switches idle GPUs to that type through AMD SMI at startup. GPUs without a
  compute partition type (RDNA, SR-IOV VFs, embedded APUs) stay CU-maskable.

In the register annotation (`hami.io/node-amd-register`) soft entries have
`Mode` empty and hard entries carry the compute partition type, the way HAMi
branches on the NVIDIA `MigMode`.
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

## Configuration

The most common Helm values:

| Value | Default | Description |
|---|---|---|
| `dp.splitCount` | `0` | How many pods may share one GPU. `0` picks it per GPU: 2 on gfx12, otherwise 10. |
| `dp.allocatorPolicy` | `besteffort` | How kubelet picks GPUs for multi-GPU pods: `besteffort` (same as `binpack`, closest GPUs) or `spread` (farthest GPUs). |
| `dp.dmemBackend` | `true` | Hard VRAM cap through the dmem cgroup. It switches itself off on nodes without dmem or the systemd driver. |
| `dp.cdi.enabled` | `false` | Inject GPUs through CDI instead of device nodes. |
| `dp.muslFailClosed.enabled` | `false` | Refuse a slice whose image cannot load the memory hook (musl, static, or glibc older than 2.34), unless dmem caps it. Experimental. |
| `dp.healthPulse` | `10` | Seconds between GPU health checks. `0` disables them. |

See the [chart README](helm/amd-gpu/README.md) for all values and the [configuration guide](docs/user-guide/configuration.md) for the matching plugin flags.

## Limits

- **CU slices are cooperative.** ROCm applies the mask inside the process. The hook pins `HSA_CU_MASK` to the pod spec, so `setenv` or `os.environ` cannot widen it. A process that re-executes itself without `LD_AUDIT`, however, runs outside the slice. Do not rely on CU slices to isolate untrusted tenants until KFD enforces a CU limit ([#55](https://github.com/Project-HAMi/amd-device-plugin/issues/55)).
- **Memory limits need glibc 2.34 or newer** because the hook loads through `LD_AUDIT`. musl and static images never load the hook, so only dmem caps them, and only where dmem is available. On older glibc the dynamic linker fails to load the hook (`GLIBC_2.34 not found`) and the container does not start. With `dp.muslFailClosed.enabled` the plugin checks the image first: it leaves the hook out when dmem caps the slice, and refuses the pod otherwise.
- **gfx12 shares poorly beyond 2 pods.** These GPUs have few compute queues, so more than about 2 processes per GPU can lose most of their throughput, depending on the model and firmware: an Ollama qwen2.5:3b pair lost about 83%, while a plain kernel loop split the card evenly across 6 processes with no loss in total. This is why `dp.splitCount` defaults to 2 there ([#54](https://github.com/Project-HAMi/amd-device-plugin/issues/54)).

## Tested hardware

| GPU | Architecture | Validated |
|---|---|---|
| AMD Instinct MI300X VF | CDNA3 | Registration (UUID, product, VRAM, CUs) |
| Radeon RX 9060 XT | RDNA4, gfx1200 | Registration, health, CU and memory slices, CU-mask pinning, plugin restart |
| Radeon RX 9070 XT + Radeon iGPU | RDNA4 gfx1201 + RDNA2 gfx1036 | Multi-GPU ordering, per-GPU memory and dmem caps, WGP slices, CDI, GPU removal |

RDNA3 (gfx11) and CDNA compute or memory slicing have not been validated yet.

## Development

The plugin uses cgo against AMD SMI, libdrm and hwloc, so build and test it in the builder image:

```bash
docker build --target builder -t amd-device-plugin-builder .
docker run --rm -e LD_LIBRARY_PATH=/opt/rocm/lib \
  --workdir /go/src/github.com/Project-HAMi/amd-device-plugin amd-device-plugin-builder \
  bash -c 'ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.26 && go test ./...'
helm lint helm/amd-gpu
```

Tests that need a GPU skip automatically when none is present. Changes to discovery, AMD SMI, ROCr visibility, CU masks or memory limits also need a run on a real GPU node. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security and license

Report vulnerabilities as described in [SECURITY.md](SECURITY.md). Licensed under [Apache 2.0](LICENSE).
