# HAMi AMD Device Plugin

[![CI](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/Project-HAMi/amd-device-plugin)](LICENSE)

This repository contains the AMD device plugin used by [HAMi](https://github.com/Project-HAMi/HAMi) to discover AMD GPUs and enforce HAMi vGPU allocations on Kubernetes nodes. It is based on AMD's upstream [ROCm Kubernetes device plugin](https://github.com/ROCm/k8s-device-plugin).

## Current capabilities

- Uses the AMD SMI C API through cgo to get device UUID implemented by building with ROCm base-image.
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
docker build -t ghcr.io/project-hami/amd-device-plugin:dev .
```

The Docker build compiles the cgo code against the ROCm 7.0.2 AMD SMI SDK and temporarily packages the checked-in `libamvgpu.so`. CI verifies that the hook exists and uses the same Dockerfile for the published image, so a missing hook fails the image build.

## Deploy with Helm

The device-plugin image currently includes the repository's `libamvgpu.so` under `/opt/hami/lib/amd`, separate from the host-mounted destination. Following HAMi's hook-delivery model, the device-plugin container mounts the node's `<hostHookPath>/vgpu` directory and runs `amd-vgpu-init.sh` from a `postStart` lifecycle hook. The script compares the bundled and installed files and atomically updates `<hostHookPath>/vgpu/libamvgpu.so` when needed. With the default `hostHookPath=/usr/local`, Allocate then mounts `/usr/local/vgpu/libamvgpu.so` from the host into workload containers.

This bundled binary is a temporary delivery mechanism. It will be replaced by an artifact obtained from the official `amd-hami-core` repository once that project provides a release and consumption pipeline. Set `dp.hookInstaller.enabled=false` only when the hook is managed on every node by another mechanism.

```bash
helm dependency build ./helm/amd-gpu
helm upgrade --install amd-gpu ./helm/amd-gpu \
  --namespace kube-system \
  --create-namespace \
  --set dp.image.repository=ghcr.io/project-hami/amd-device-plugin \
  --set dp.image.tag=main
```

Images are published to GitHub Container Registry after CI succeeds on `main` and version tags. The GHCR package must be public for deployment without credentials; otherwise configure `imagePullSecrets`.

Verify registration:

```bash
kubectl get node <node-name> -o jsonpath='{.metadata.annotations.hami\.io/node-amd-register}'
```

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
  --workdir /go/src/github.com/ROCm/k8s-device-plugin \
  --env LD_LIBRARY_PATH=/opt/rocm/lib \
  amd-device-plugin-builder:test \
  bash -c 'ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.26 && go test ./...'

helm dependency build ./helm/amd-gpu
helm lint ./helm/amd-gpu
helm template amd-gpu ./helm/amd-gpu --namespace kube-system >/dev/null
```

Hardware-dependent tests skip automatically when no AMD GPU is present. Real-node validation is still required for changes to device discovery, AMD SMI calls, ROCr visibility, CU masks, or memory interception.

## License

Apache License 2.0. See [LICENSE](LICENSE).
