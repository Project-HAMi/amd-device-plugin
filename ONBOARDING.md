# Onboarding: Project-HAMi/amd-device-plugin

A short guide for someone who opens the repo for the first time. For what was tested on a real GPU and what is still open, read [STATUS.md](STATUS.md). It also covers [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core).

## 1. What it is

- A Kubernetes device plugin that lets the [HAMi](https://github.com/Project-HAMi/HAMi) scheduler share AMD GPUs between pods. It started from AMD's ROCm device plugin. Everything is pre-1.0 (`0.0.x`), Apache 2.0.
- It publishes each GPU of a node in the node annotation `hami.io/node-amd-register`.
- When a pod starts it limits the pod to the compute units (CUs) and VRAM that HAMi gave it:
  - CUs: the environment variable `HSA_CU_MASK` (the ROCm runtime enforces it). On RDNA a slice is whole WGPs (pairs of CUs).
  - VRAM: the library `libamvgpu.so` (from amd-hami-core, loaded with `LD_AUDIT`) and, on kernel 6.14 or newer, a hard cap through the dmem cgroup.
- Whole-GPU requests get no hook and no CU limit, so any image works for them.
- Three binaries: `k8s-device-plugin` (the plugin), `k8s-vgpu-monitor` (optional Prometheus metrics) and `k8s-node-labeller` (optional `amd.com/gpu.*` node labels, from upstream).

## 2. How a pod gets its GPU

1. **Webhook (HAMi).** Checks `amd.com/gpucores` is 1 to 100 and sends the pod to the HAMi scheduler.
2. **Scheduler (HAMi).** Reads the node annotation, picks node and GPUs, writes `hami.io/amd-devices-to-allocate` and `hami.io/amd-devices-allocated` on the pod. One slot per container, init containers first, then app containers; the plugin maps a slot back to its container (`GetNextDeviceRequest`).
3. **Bind (HAMi).** Takes the node lock (`hami.io/mutex.lock`) and sets `hami.io/bind-phase=allocating`.
4. **Kubelet** calls this plugin's `Allocate`.
5. **Plugin `Allocate`** (`internal/pkg/plugin/plugin.go`) finds the pod, turns the allocation into devices (`/dev/kfd`, `/dev/dri/*`, or CDI names), `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES` and `HSA_CU_MASK`. For slices it adds `HIP_DEVICE_MEMORY_LIMIT_<i>`, `LD_AUDIT` and a read-only mount of the hook, records the CU bits in `hami.io/amd-cu-allocated` and starts the dmem cap. It then sets `bind-phase=success` and releases the lock (on error: `failed`).
6. If no HAMi-tracked pod is pending (a pod that HAMi did not schedule), it serves kubelet's ids as whole GPUs.

Every 30 s the plugin re-publishes the node annotation; every `-pulse` seconds it re-sends device health to kubelet.

## 3. Resources a pod can ask for

| Resource | Meaning | Enforced by |
|---|---|---|
| `amd.com/gpu` | Number of GPUs. Kubelet sees one device per share: allocatable is GPUs x share count (10, or 2 on gfx12). | HAMi `Fit`, then this plugin |
| `amd.com/gpucores` | Percent of each GPU's CUs, 1 to 100. Omitted means 100. | HAMi books it, plugin sets `HSA_CU_MASK` |
| `amd.com/gpumem` | VRAM per GPU in MiB. Omitted means all of it. | HAMi books it, hook and dmem cap enforce it |
| `amd.com/gpumem-percentage` | Share of the GPU's memory, 0 to 100 (HAMi side). | HAMi turns it into MiB |

Notes:

- A request with only `gpumem` still books 100 percent of the cores, so only one such pod runs at a time (HAMi #3192, merged on 2026-10-13, adds `defaultCores` and `defaultMemory` settings).
- Optional pod annotations the HAMi scheduler honors: `amd.com/use-gputype`, `amd.com/nouse-gputype`, `amd.com/use-gpu-uuid`, `amd.com/nouse-gpu-uuid`, `amd.com/numa-bind`, `hami.io/gpu-scheduler-policy`.
- With `-resource_naming_strategy=mixed`, partitioned Instinct GPUs get names like `amd.com/cpx_nps4`.
- Do not put whole-GPU pods and slices on one card when VRAM isolation matters.

## 4. What the plugin publishes

The node annotation `hami.io/node-amd-register` is a JSON list, one entry per GPU:

| Field | Value |
|---|---|
| `id` | AMD SMI UUID (or `GPU-<unique_id>` for an XCP partition). Same in both operating modes. |
| `index`, `numa`, `type`, `health` | DRM card minor, NUMA node, market name, false if the DRM node cannot be opened or the exporter says unhealthy |
| `count` | Share count: `-split_count`, else 2 on gfx12 and 10 elsewhere |
| `devmem`, `devcore` | VRAM in MiB (times `-device_memory_scaling`), active CUs |
| `custominfo` | `pciBDF`, `cuPerWGP` (2 on RDNA), `computeQueues`, `xgmiPeers` (BDFs reachable over XGMI), partition profiles |

Other annotations: `hami.io/amd-operating-mode` and `hami.io/amd-compute-partition` (node, per-node overrides of `OPERATING_MODE` and `COMPUTE_PARTITION`), `hami.io/device-cordon` (node, HAMi skips the listed GPUs; needs HAMi #3192, still open).

Extras: Events `AMDGPUUnhealthy`, `AMDGPUMissing`, `AMDGPURecovered`, `AMDGPUModeChangedInUse`; with `-report_node_capacity` the node also gets `amd.com/gpumem` and `amd.com/gpucores` capacity. When no GPU is left to serve, the annotation is cleared.

## 5. amd-hami-core and `libamvgpu.so`

- An `LD_AUDIT` library (`src/hip/libamvgpu_audit.c`). It wraps the HIP allocation calls (`hipMalloc`, `hipMallocAsync`, `hipMalloc3D`, arrays, pools, VMM and more) and returns out-of-memory past the limit. `hipMemGetInfo` and device properties report the limit as the total.
- Accounting is shared between processes through an mmap'd file (`/tmp/hipdevshr.cache`); a reservation is made before the real allocation, so concurrent allocations cannot all pass.
- It does not mask CUs. It pins `HSA_CU_MASK`, `AMD_TASK_PRIORITY` and `HIP_OVERSUBSCRIBE` to the pod-spec values (read from `/proc/1/environ`), so a process cannot widen its slice with `setenv`.
- Variables: `LD_AUDIT`, `HIP_DEVICE_MEMORY_LIMIT_<i>` (for example `4096m`), `HIP_OVERSUBSCRIBE`, `AMD_TASK_PRIORITY` (0 high class, higher is low; set by the user), `LIBHIP_LOG_LEVEL` (1 to 4), `HIP_DEVICE_MEMORY_SHARED_CACHE`, `ACTIVE_OOM_KILLER`.
- Limits: a process that loses `LD_AUDIT` (for example `env -i`), static binaries and musl images run without the hook (the dmem cap still holds, but `hipMallocAsync` above it aborts the process). The build baseline is glibc 2.34. `hipMallocMipmappedArray` is not intercepted. At most 64 devices and 1024 processes per container.

## 6. Install and main settings

```
helm upgrade --install amd-gpu ./helm/amd-gpu --namespace kube-system --create-namespace
```

The HAMi scheduler must be installed first (with its own device plugin disabled if there are no NVIDIA GPUs). The chart and the image must come from the same commit or release: the image tag defaults to the chart `appVersion`, a published release, and a chart from `main` passes flags a published image does not know. Remove any other plugin that registers `amd.com/gpu` (kubelet refuses the second one).

Main chart values (all under `dp.` unless noted; the full list is in `helm/amd-gpu/values.yaml` and `docs/user-guide/configuration.md`):

| Value | Flag or env | Default | What it does |
|---|---|---|---|
| `image.repository`, `image.tag` | | ghcr.io/project-hami/amd-device-plugin, chart appVersion | Image. |
| `splitCount` | `-split_count` | 0 (10, or 2 on gfx12) | Pods that may share a GPU. Negative values exit with an error. |
| `operatingMode`, `computePartition` | `OPERATING_MODE`, `COMPUTE_PARTITION` | `cu` | `partition` uses the hardware compute partitions of Instinct GPUs. |
| `allocatorPolicy` | `-allocator_policy` | `besteffort` | Multi-GPU choice: `besteffort`, `binpack`, `spread`. |
| `dmemBackend` | `-dmem_backend` | true | Kernel dmem cap (needs kernel 6.14+, systemd cgroup driver). |
| `excludeGPUs` | `-exclude_gpus` | none | PCI BDFs or `cardN` the plugin leaves out. |
| `deviceMemoryScaling` | `-device_memory_scaling` | 1 | Above 1 oversubscribes through managed memory. |
| `reportNodeCapacity` | `-report_node_capacity` | false | Publish gpumem and gpucores as node capacity. |
| `cdi.enabled`, `cdi.specDir` | `-cdi_spec_dir` | off | Inject GPUs through CDI. Turning it off leaves `amd.com-gpu.json` behind. |
| `muslFailClosed.*` | `-musl_fail_closed` | off | Refuse a slice whose image cannot load the hook. Experimental; the path defaults are RKE2, k3s needs other values. |
| `hipLogLevel`, `logVerbosity`, `healthPulse` | `-hip_log_level`, `-v`, `-pulse` | 0, 2, 10 | Logging and health interval. |
| `monitor.enabled` (top level) | | false | Run `k8s-vgpu-monitor` (metrics on `:9394`). |

The plugin needs `NODE_NAME` and RBAC to patch nodes, `nodes/status` and pods and to create events; the chart grants both. The labeller needs `DS_NODE_NAME`. The AMD Device Metrics Exporter is optional: if its socket is at `/var/lib/amd-metrics-exporter/`, its per-GPU health is used.

## 7. Repo layout

```
cmd/k8s-device-plugin, k8s-vgpu-monitor, k8s-node-labeller
internal/pkg/  allocator  amdgpu  amdsmi  cuallocation  dmem  exporter  hwloc
               libcheck  plugin  utils  vgpumonitor
helm/amd-gpu/  chart            scripts/amd-vgpu-init.sh  hook installer (postStart)
docs/          Sphinx docs      example/  pod and deployment examples
amd-hami-core/ git submodule (run `git submodule update --init`)
Dockerfile, ubi-dp.Dockerfile, labeller.Dockerfile, ubi-labeller.Dockerfile
```

## 8. Build and test

- Images: `make build-device-plugin` (also `build-labeller`, `build-ubi-device-plugin`, `build-ubi-labeller`, `make all`). The main image is based on `rocm/dev-ubuntu-24.04:7.2.4`, builds the hook from the submodule and bundles both binaries.
- The Go packages use cgo (libdrm, hwloc, AMD SMI), so tests run in the builder image:

```
docker build --target builder -t amd-device-plugin-builder .
docker run --rm -e LD_LIBRARY_PATH=/opt/rocm/lib --workdir /go/src/github.com/Project-HAMi/amd-device-plugin \
  amd-device-plugin-builder bash -c 'ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.27 && go vet ./... && go test -race ./...'
helm lint helm/amd-gpu
```

- Some tests need a GPU, debugfs or the amdgpu driver and skip without them. CI has no GPU.
- amd-hami-core needs ROCm, CMake, make and gcc. `make -f Makefile.hip` builds `build-hip/libamvgpu.so`; `make -f Makefile.hip test` builds the on-GPU test. Unit tests are plain `gcc` programs in `test/` (also run under ASan, UBSan and ThreadSanitizer in CI), and `test/check_glibc_abi.sh` checks the glibc 2.34 baseline. To build without installing ROCm: `docker build -f Dockerfile.hip --build-arg ROCM_IMAGE=<image> -t libamvgpu-builder .`, then `docker run --rm -v $(pwd)/dist:/dist libamvgpu-builder` (`dist/libamvgpu.so`, `dist/test_memory_limit`). A fuzz target for the size parser runs in ClusterFuzzLite.
- Docs: `docs/` is a Sphinx site built by Read the Docs (`docs/sphinx/_toc.yml.in` is the table of contents).

## 9. Contributing

- Commits are signed off (`git commit -s`). Open pull requests with the labels and `/kind` line from `.github/PULL_REQUEST_TEMPLATE.md`; a change needs a test that fails without it. Security reports go through GitHub Security Advisories (`SECURITY.md`).
- Both repositories keep `main` protected: CI must pass; repository admins can merge directly.

## 10. Verified on a real GPU

Everything was run on one AMD Instinct MI325X (shown as a virtual function) with k3s, the HAMi scheduler, kernel 6.8 and 7.0. [STATUS.md](STATUS.md) has the result for every pull request and what is not verified (several GPUs and XGMI, real partition flips, RDNA cards, memory spill to host, a real hardware fault).

## 11. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Pod stays Pending with `CardInsufficientCore` or `CardInsufficientMemory` | Another pod holds the cores or memory. A pod without `gpucores` books 100 percent. |
| Pod stays Pending with `NodeInsufficientDevice` | Allocatable GPUs are 0 (plugin down or the GPU unhealthy), or the request needs more distinct GPUs than the node has. |
| Pod fails with `no binding pod found` | An older plugin got a pod HAMi did not schedule; current versions serve whole GPUs. |
| Slice Pod with an init container gets the whole GPU (log: `annotation allocation unavailable`) | The plugin predates #189, which maps the annotation slots past init containers. |
| Plugin Pod stops with a usage message | Chart and image come from different releases. |
| Allocatable is 0 right after a restart | Wait one registration (30 s); check that no other plugin owns `amd.com/gpu`. |
| Slice runs without a memory limit | The image cannot load the hook (musl, static, glibc older than 2.34). With `muslFailClosed` it is refused instead. |
| Pod using `hipMallocAsync` aborts above the limit | Without the hook only the dmem cap applies and the ROCm runtime aborts. |
| `hami.io/mutex.lock` stays on the node | A bind did not finish; it expires after the HAMi lock timeout (5 minutes by default). |

## 12. Glossary

- **CU**: compute unit. **WGP**: workgroup processor, a pair of CUs on RDNA.
- **CDNA / RDNA**: AMD's datacenter (Instinct) and consumer GPU families. **gfx**: ISA name, for example gfx942 (MI300 family), gfx12 (RDNA4).
- **SPX, DPX, QPX, CPX**: compute partition modes (1, 2, 4, 8 partitions). **NPS**: memory partition mode. **XCP/XCC**: a partition and its compute die.
- **XGMI**: the GPU to GPU link. **VF**: an SR-IOV virtual function, cannot change its partition.
- **dmem**: the kernel cgroup controller that caps VRAM per cgroup. **CDI**: Container Device Interface.
- **HAMi**: the scheduler and webhook that books the shares. **Hook**: `libamvgpu.so`.
