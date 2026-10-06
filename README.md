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
- **gfx12 shares poorly beyond 2 pods.** These GPUs have few compute queues, so more than about 2 processes per GPU lose most of their throughput. This is why `dp.splitCount` defaults to 2 there ([#54](https://github.com/Project-HAMi/amd-device-plugin/issues/54)).

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
