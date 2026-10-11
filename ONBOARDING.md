# Onboarding guide: Project-HAMi/amd-device-plugin

## In one minute

- This repo is a Kubernetes device plugin that lets the HAMi scheduler share AMD GPUs between pods.
- It publishes each GPU of a node in the node annotation `hami.io/node-amd-register`.
- When a pod starts, it limits the pod to the compute units (CUs) and VRAM that HAMi gave it.
- CU limits use `HSA_CU_MASK`. VRAM limits use the `libamvgpu.so` hook and, where the kernel allows, the dmem cgroup.
- It also ships an optional Prometheus monitor and a node labeller. Everything is pre-1.0 (`0.0.x`).

This guide is for someone who opens the repo for the first time. Paths are relative to the repo root unless noted. Where a README or doc disagrees with the code, this guide follows the code and says so in a "Note".

The behavior was also checked on a real AMD Instinct MI325X (one GPU, as a virtual function) with k3s and the HAMi scheduler. Section 11.1 lists what that pass found and what it could not check.

---

## 1. What the project is

The repo is a Kubernetes device plugin. It lets the HAMi scheduler share AMD GPUs between pods. It is derived from AMD's upstream ROCm Kubernetes device plugin. See the first paragraph of `README.md` and the Apache/AMD copyright headers in `cmd/k8s-device-plugin/main.go` and `internal/pkg/amdgpu/amdgpu.go`.

What it does (`README.md`, "What it does"; code references are in section 2):

- It registers every AMD GPU of a node in the node annotation `hami.io/node-amd-register`.
  - The entry holds UUID, product name, VRAM in MiB, CU count, NUMA node, PCI BDF and more. Section 2.3 lists all fields.
- When a pod starts, it limits the pod to the CUs and VRAM that the HAMi scheduler gave it.
  - Compute: the environment variable `HSA_CU_MASK`, which the ROCm runtime enforces. On RDNA (gfx10 and later), slices are whole WGPs (pairs of CUs).
  - Memory: a per-GPU limit `HIP_DEVICE_MEMORY_LIMIT_<i>`. The library `libamvgpu.so` enforces it inside the process. The library is built from the amd-hami-core repo and loaded through `LD_AUDIT`.
  - Where the node supports it, there is also a hard kernel cap through the dmem cgroup controller.
- It handles multi-GPU pods. Device order, CU masks and memory limits all use one container-local order.
- It saves CU allocations in pod annotations. They survive a plugin restart.
- It marks a GPU unhealthy when its DRM device cannot be opened.
- It also uses health from the AMD Device Metrics Exporter when that is installed.

Whole-GPU requests get no memory hook and no CU restriction. So any image works for them. See the end of "What it does" in `README.md`. In code, see the `!p.isWholeGPU(devreq)` guard in `Allocate` in `internal/pkg/plugin/plugin.go`.

The repo ships three binaries. All are built from one module (`go.mod`: module `github.com/Project-HAMi/amd-device-plugin`, `go 1.26.9`).

| Binary | Purpose |
|---|---|
| `k8s-device-plugin` | The device plugin itself (DaemonSet). |
| `k8s-vgpu-monitor` | Optional Prometheus exporter for per-container VRAM and host GPU metrics. |
| `k8s-node-labeller` | Node labeller carried over from upstream. It adds `amd.com/gpu.*` labels. |

Versions:

- The chart `version` and `appVersion` are `0.0.3` (`helm/amd-gpu/Chart.yaml`).
- The Makefile default `IMAGE_VERSION` is `0.0.3`.
- `CHANGELOG.md` has entries v0.0.1 to v0.0.3.
- All of it is pre-1.0. `SECURITY.md` lists `0.0.x` as the supported version line.

Licence: Apache 2.0 (`LICENSE`). Security reports go through GitHub Security Advisories, not public issues (`SECURITY.md`, `.github/ISSUE_TEMPLATE/config.yml`).

---

## 2. How the pieces fit

### 2.1 Cast of characters

| Piece | Where it lives | Role |
|---|---|---|
| kubelet device plugin API (v1beta1) | `k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1`, used in `internal/pkg/plugin/plugin.go` | The gRPC contract: `ListAndWatch`, `Allocate`, `GetPreferredAllocation`, `GetDevicePluginOptions`, `PreStartContainer`. The library `github.com/kubevirt/device-plugin-manager` (`dpm`) handles the plugin lifecycle (socket, registration with kubelet). It is wired in `cmd/k8s-device-plugin/main.go`. |
| HAMi admission webhook and scheduler | HAMi repo: `pkg/scheduler/webhook.go`, `pkg/scheduler/scheduler.go`, `pkg/device/amd/device.go` | Sends pods that request AMD resources to the HAMi scheduler. Reads the node annotation. Picks the node and the GPUs. Writes the allocation into pod annotations. Takes the node lock at bind time. |
| This device plugin | `cmd/k8s-device-plugin`, `internal/pkg/*` | Publishes the node annotation. Answers kubelet. Turns the scheduler's allocation into devices, environment and mounts. |
| `libamvgpu.so` | amd-hami-core: `src/hip/libamvgpu_audit.c`, `src/multiprocess/hip_multiprocess_memory_limit.c` | An `LD_AUDIT` library injected into the pod. It intercepts HIP allocation calls and enforces the per-pod VRAM limit. It also pins a few environment variables to the pod spec. |
| Monitor | `cmd/k8s-vgpu-monitor`, `internal/pkg/vgpumonitor` | Optional Prometheus exporter. |
| Labeller | `cmd/k8s-node-labeller` | Optional node labels. Independent of HAMi. |

### 2.2 Request flow

```
 user: kubectl apply pod with limits amd.com/gpu, amd.com/gpucores, amd.com/gpumem
   |
   v
 [1] HAMi mutating webhook (HAMi pkg/scheduler/webhook.go)
     - per container: AMDDevices.MutateAdmission (HAMi pkg/device/amd/device.go)
     - validates amd.com/gpucores is an integer 1..100
     - sets pod.spec.schedulerName to the HAMi scheduler
     - denies the pod if a container is privileged (webhook.go, hasPrivileged && hasResource)
   |
   v
 [2] kube-scheduler -> HAMi scheduler extender: Filter
     - reads node annotation hami.io/node-amd-register (published by THIS plugin)
     - AMDDevices.Fit: health, type, UUID, Count (sharers), memory, cores
     - writes pod annotations hami.io/vgpu-node, hami.io/amd-devices-to-allocate,
       hami.io/amd-devices-allocated   ("uuid,type,usedmem,usedcores:" per device,
       ";" between containers)
   |
   v
 [3] HAMi scheduler Bind (HAMi pkg/scheduler/scheduler.go)
     - takes the node lock: node annotation hami.io/mutex.lock = "<RFC3339 time>,<ns>,<pod>"
     - patches pod: hami.io/bind-phase=allocating, hami.io/bind-time=<unix seconds>
     - binds the pod to the node
   |
   v
 [4] kubelet on the node: device manager picks kubelet device ids of amd.com/gpu
     - calls GetPreferredAllocation (this plugin: allocator policy besteffort|binpack|spread)
     - calls Allocate(device ids)                                  --> THIS PLUGIN
   |
   v
 [5] THIS PLUGIN, Allocate (internal/pkg/plugin/plugin.go)
     a. find the pod: node lock owner (utils.GetAllocatePodByNode), else a Pending pod on the
        node with bind-time, bind-phase=allocating and hami.io/vgpu-node=<node>
     b. rebuild CU occupancy of the node from pods' hami.io/amd-cu-allocated annotations
     c. decode hami.io/amd-devices-to-allocate for the next container that has devices, then
        erase that container's entry from the annotation
        (no usable annotation, a different device count, or no HAMi-tracked pod on the node
         at all: fall back to kubelet's ids, treated as whole GPUs)
     d. per device: resolve the published id (AMD SMI UUID, ROCr UUID of an XCP partition,
        "<bdf>#<slot>", or "AMDGPU-<n>") to card/renderD and ROCr UUID
     e. build the response: /dev/kfd + /dev/dri/card<N> + /dev/dri/renderD<M> (or CDI names),
        env ROCR_VISIBLE_DEVICES, HIP_VISIBLE_DEVICES, HSA_CU_MASK; for slices also
        HIP_DEVICE_MEMORY_LIMIT_<i>, LD_AUDIT, read-only mount of libamvgpu.so
     f. sliced pods: patch hami.io/amd-cu-allocated on the pod; start the dmem cap goroutine
     g. when no container of the pod has devices left to allocate: bind-phase=success and
        release the node lock (on any error: bind-phase=failed and release the lock)
   |
   v
 [6] container runtime starts the container with that response
     - ROCm reads HSA_CU_MASK / ROCR_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES
     - the dynamic linker loads /usr/local/vgpu/libamvgpu.so (LD_AUDIT) for slices
     - libamvgpu wraps hipMalloc & co, counts bytes in a shared file (default
       /tmp/hipdevshr.cache) and returns hipErrorOutOfMemory past the limit
     - in the background the plugin writes "drm/<bdf>/vram <bytes>" into the pod cgroup's
       dmem.max (kernel-enforced cap) where dmem is usable

 Parallel, every 30 s (5 s after a failure): the plugin re-publishes hami.io/node-amd-register
 (WatchAndRegister). Every -pulse seconds: ListAndWatch re-sends kubelet device health.
```

Annotation keys involved. The constants live in `internal/pkg/utils/types.go`, `internal/pkg/utils/nodelocks.go` and `internal/pkg/plugin/plugin.go`.

| Key | On | Written by | Meaning |
|---|---|---|---|
| `hami.io/node-amd-register` | node | plugin | JSON list of registered devices (section 2.3). |
| `hami.io/mutex.lock` | node | HAMi scheduler (create), plugin or scheduler (release) | Per-node bind lock. The value is `time,namespace,pod`. Expiry is a HAMi scheduler matter: `NodeLockTimeout`, default 5m, set by env `HAMI_NODELOCK_EXPIRE` (HAMi `pkg/util/nodelock/nodelock.go`). The plugin has a copy of that code and parses the same env into `utils.NodeLockTimeout`. Nothing in this repo reads that value. |
| `hami.io/vgpu-node` | pod | HAMi scheduler | The chosen node. |
| `hami.io/bind-time`, `hami.io/bind-phase` | pod | scheduler, then plugin | The phase is `allocating`, then `success` or `failed`. |
| `hami.io/amd-devices-to-allocate` | pod | scheduler. The plugin erases consumed containers. | Work queue for `Allocate`. |
| `hami.io/amd-devices-allocated` | pod | scheduler | The full allocation, kept for accounting. The plugin also uses it to find pods that hold a GPU when it warns about mode changes (`podsHolding`). |
| `hami.io/amd-cu-allocated` | pod | plugin | JSON `{"<device id>": "<CU id list>"}`, for example `{"<uuid>":"0-3,8"}`. It is the source of truth for CU occupancy after a restart. |
| `hami.io/amd-operating-mode` | node | operator | Per-node override of `OPERATING_MODE` (`cu` or `partition`). Read once at plugin start. |
| `hami.io/amd-compute-partition` | node | operator | Per-node override of `COMPUTE_PARTITION`. Read once at plugin start. |

### 2.3 The node annotation `hami.io/node-amd-register`

The annotation is a JSON array of `utils.DeviceInfo` (`internal/pkg/utils/types.go`). The HAMi side decodes the same shape (`device.DeviceInfo` in HAMi `pkg/device/devices.go`). HAMi also forces `DeviceVendor` to `AMD` in `GetNodeDevices`. All fields use `omitempty`. So zero values (`health: false`, `index: 0`, `numa: 0`, empty `mode`) are left out.

| JSON field | Go field | Value as the plugin fills it (`getAPIDevices`, `internal/pkg/plugin/plugin.go`) |
|---|---|---|
| `id` | `ID` | Whole GPU: the AMD SMI UUID. XCP partition (partition mode on kernels that expose them): the ROCr UUID `GPU-<16 hex digits of KFD unique_id>`. The same id is used in both operating modes. |
| `index` | `Index` | The DRM card minor (`card<N>`). |
| `count` | `Count` | How many pods may share the device. It is `-split_count`. Per GPU it is 2 when `gfx_target_version >= 120000`, otherwise 10 (`splitCountFor`). It is forced to 1 for hard partition entries. |
| `devmem` | `Devmem` | VRAM in MiB from libdrm (`AMDGPU_INFO_MEMORY` vram_size / 1 MiB), multiplied by `-device_memory_scaling` (`scaledMemory`). XCP partitions get floor(VRAM / partitions). |
| `devcore` | `Devcore` | Active CU count from libdrm (`cu_active_number`). XCP partitions get floor(CUs / partitions). |
| `type` | `Type` | The AMD SMI market name. It is `amd-gpu` if that is unavailable. |
| `numa` | `Numa` | NUMA node from sysfs `numa_node`. |
| `mode` | `Mode` | Empty (a "soft" entry) in cu mode. In partition mode it is the compute partition type (`spx`, `dpx`, `qpx`, `cpx`) for GPUs that report one (a "hard" entry). |
| `health` | `Health` | `true` unless the exporter reports the GPU unhealthy (`exporter.UnhealthyGPUs`). |
| `devicevendor` | `DeviceVendor` | `amd`. |
| `custominfo.pciBDF` | `CustomInfo` | Standard PCI spelling in lower case, for example `<bdf>`. |
| `custominfo.cuPerWGP` | | 2 when `gfx_target_version >= 100000`, else 1. |
| `custominfo.computeQueues` | | `num_cp_queues` from the KFD node. Only set when greater than 0. |
| `custominfo.partitionProfile` | | XCP entries only: `<compute type>_<memory type>`. |
| `custominfo.partitionProfiles` | | Whole GPUs whose AMD SMI partition profiles could be read: a list of profile objects. Not published for GPUs that cannot change their partition (SR-IOV VFs). `XCCPerPartition` is the GPU's XCC total split over the profile's partitions (8, 4, 2, 1 on an 8 XCC GPU), because AMD SMI lists the XCC count for the current profile only. |
| `custominfo.xgmiPeers` | | Sorted BDFs of the other registered GPUs that are reachable over XGMI links (KFD link type 11). Left out when empty. |

Note: `README.md` says each `partitionProfiles` entry has `profile_index` and similar snake case keys. But `amdsmi.PartitionProfile` (`internal/pkg/amdsmi/amdsmi.go`) has no JSON tags. So the annotation carries the Go field names `ProfileIndex`, `Type`, `MemoryCaps`, `NumPartitions` and `XCCPerPartition`. The spelling `profile_index` is from the `amd-smi` CLI JSON (see `testdata/amdsmi-partition-g*-mi355x.json`).

Note: in the HAMi checkout used for this guide, the scheduler reads only `custominfo.cuPerWGP`. See `cuPerWGP` in HAMi `pkg/device/amd/device.go`. A grep for `xgmiPeers` and `computeQueues` in HAMi `pkg` and `cmd` finds nothing. The other fields are published for scheduler use. The monitor uses `pciBDF`.

Registration extras (`RegisterInAnnotation`):

- `WatchAndRegister` refreshes the annotation every 30 s. It retries after 5 s on error.
- With `-report_node_capacity`, it also patches `status.capacity` and `status.allocatable` of the node.
  - It sets `amd.com/gpumem` (MiB) and `amd.com/gpucores`, summed over healthy GPUs (`healthyCapacity`, `utils.PatchNodeStatusCapacity`).
  - With the flag off, it removes those two entries if an earlier run left them (`utils.ClearNodeStatusCapacity`). Kubelet keeps extended resources that it did not advertise itself.
- It records node Events in namespace `default`, with source component `amd-device-plugin`.
  - `AMDGPUUnhealthy`, `AMDGPUMissing` and `AMDGPURecovered` fire when a GPU changes state between registrations.
  - `AMDGPUModeChangedInUse` fires when the operating mode changed while pods still hold a GPU (`internal/pkg/plugin/health_events.go`).
  - The first registration after a restart only records a baseline.
- A GPU is left out of the registration in these cases. The log lines say `skip GPU ...`.
  - libdrm cannot read its capacity.
  - It has no render node.
  - AMD SMI returns no UUID for a whole GPU.
- When no GPU is left to serve, `ClearRegistration` rewrites the annotation to an empty list. Then the scheduler stops placing pods there.

### 2.4 amd-hami-core and `libamvgpu.so`

Repo layout of amd-hami-core:

- `src/hip/libamvgpu_audit.c`: the audit library.
- `src/multiprocess/hip_multiprocess_memory_limit.c` and `.h`: the shared accounting region.
- `src/include/*.h`: log, glibc compat, memory size parsing, `/proc` reading.
- `src/hip/*.h`: pure helpers (`alloc_tracker.h`, `alloc_size.h`, `env_policy.h`, `oversubscribe.h`, `queue_priority.h`).
- `src/hip/test_memory_limit.c`: the on-GPU test program.
- `test/`: unit tests and `check_glibc_abi.sh`.
- `Makefile.hip`, `CMakeLists.txt.hip`, `Dockerfile.hip`.

How it works (`libamvgpu_audit.c`):

- It implements the `LD_AUDIT` interface (`la_version`, `la_objopen`, `la_symbind64`).
- It identifies `libamdhip64` and `libhsa-runtime64`. It rebinds calls from non-HIP callers to wrappers.
- This is why LD_PRELOAD is not used. The file header says LD_PRELOAD breaks HIP initialization on ROCm 7.x.
- These allocation calls get a limit check:
  - `hipMalloc`, `hipMallocManaged`, `hipMallocAsync`, `hipMallocFromPoolAsync`, `hipMallocPitch`, `hipMalloc3D`, `hipMallocArray`, `hipMalloc3DArray`, `hipMemCreate`, `hipExtMallocWithFlags`.
  - The driver-API calls `hipArrayCreate`, `hipArray3DCreate`, `hipMemAllocPitch`.
- `hipMallocMipmappedArray` is not intercepted (amd-hami-core README, "Limits").
- `hipMemGetInfo`, `hipDeviceTotalMem` and the `totalGlobalMem` of device properties report the limit as the total.
- Accounting is shared across processes through an mmap'd file.
  - The file is set by `HIP_DEVICE_MEMORY_SHARED_CACHE`. The default is `/tmp/hipdevshr.cache` (from `src/include/libamvgpu.h`).
  - It holds up to 1024 processes and 64 devices (`HIP_MAX_PROCS`, `HIP_MAX_DEVICES`).
  - A reservation is made before the real allocation. So concurrent allocations cannot all pass the check.
- It does not mask CUs. The ROCm runtime enforces `HSA_CU_MASK`. The library only pins it (see below).

Environment variables. The table below is from the amd-hami-core README, checked against `libamvgpu.h`, `env_policy.h`, `queue_priority.h`, `oversubscribe.h` and `hip_log_utils.h`.

| Variable | Set by | Effect in the library |
|---|---|---|
| `LD_AUDIT` | plugin: `/usr/local/vgpu/libamvgpu.so` (slices only) | Loads the library. |
| `HIP_DEVICE_MEMORY_LIMIT_<i>` | plugin: `<MiB>m`, one per container-local device index | Limit of device `i` (0 to 63). The value is a number with an optional K, M, G or T suffix. Invalid values mean no limit. `HIP_DEVICE_MEMORY_LIMIT` (no index) is a fallback for all devices. |
| `HSA_CU_MASK` | plugin | ROCm reads it. The library serves the pod-spec value (read from `/proc/1/environ`) even if the process changed or removed its own copy. In `env_policy.h`, the pinned variables are `HSA_CU_MASK`, `AMD_TASK_PRIORITY` and `HIP_OVERSUBSCRIBE`. |
| `HIP_OVERSUBSCRIBE` | plugin: `true` when `-device_memory_scaling` is above 1 | `true` or `1` serves `hipMalloc` from `hipMallocManaged`. |
| `AMD_TASK_PRIORITY` | the user, in the pod spec (neither the plugin nor the HAMi AMD code sets it) | `0` gives the pod's compute queues the high priority class. Any higher number gives the low class. Unset leaves the default. It is applied in `wrap_hsa_queue_create`. Measured on an MI325X, two processes on the same 25 percent slice: 144.7 kernels/s at priority 0 against 72.3 at priority 3 (108.6 each without it). HAMi pull request #3190 (open) maps `amd.com/priority` to this variable. |
| `LIBHIP_LOG_LEVEL` | plugin, when `-hip_log_level` is above 0 | 1 error, 2 warn (default), 3 info, 4 debug. |
| `HIP_DEVICE_MEMORY_SHARED_CACHE` | optional | Path of the shared accounting file. |
| `ACTIVE_OOM_KILLER` | optional | `true` reclaims slots of dead processes on OOM. |

Known gap (amd-hami-core README, "What it does", and `SECURITY.md` in this repo):

- A process that re-executes itself without `LD_AUDIT` (for example with `env -i`) runs outside the library.
- Static or musl binaries never load it.

Build baseline: glibc 2.34.

- `test/check_glibc_abi.sh` fails if the built library needs a newer GLIBC symbol version.
- `glibc_compat.h` disables the C23 `strtol` redirection to keep that baseline.
- That is why the plugin docs say workload images need glibc 2.34 or newer.

### 2.5 The monitor (`k8s-vgpu-monitor`)

The monitor is an optional DaemonSet (`monitor.enabled=true`).

- It reads the node's `hami.io/node-amd-register` annotation to name devices.
- It lists the node's pods through an informer.
- It serves `/metrics` (default `:9394`).
- Metric names match the HAMi NVIDIA vGPUmonitor, so dashboards can be reused (`internal/pkg/vgpumonitor/collector.go`).

Metrics:

- `hami_vgpumonitor_collect_success{node}`: 1 when the last scrape fully succeeded.
- Per container, from the dmem cgroup controller: `hami_vgpu_memory_used_bytes` and `hami_vgpu_memory_limit_bytes`.
  - Labels: `namespace, pod, container, vdevice_index, device_uuid`.
  - The limit is `dmem.max` where the cap exists. Otherwise it is the container's `amd.com/gpumem` limit in MiB.
- Per GPU from sysfs. The labels are `node, device_index, device_uuid, device_type`.
  - `hami_host_gpu_memory_used_bytes` (from `mem_info_vram_used`).
  - `hami_host_gpu_memory_total_bytes` (from `mem_info_vram_total`).
  - `hami_host_gpu_utilization_ratio` (from `gpu_busy_percent`).
  - `hami_host_gpu_temperature_celsius` (hwmon `temp1_input`, millidegrees to degrees).
  - `hami_host_gpu_power_usage_watts` (hwmon `power1_average`, microwatts to watts).

Requirements:

- The container metrics need cgroup v2 with the systemd cgroup driver, and a kernel with the dmem controller.
- `internal/pkg/vgpumonitor/cgroup.go` reads `<cgroupRoot>/kubepods.slice`. A missing directory gives no entries and no error.
- The monitor does not report per-container utilization.

### 2.6 The labeller (`k8s-node-labeller`)

The labeller is not HAMi-specific.

- It is a controller-runtime controller (`cmd/k8s-node-labeller/main.go`, `controller.go`).
- It labels its own node (`DS_NODE_NAME`) with `amd.com/gpu.<property>` and experimental `beta.amd.com/gpu.<property>` labels.
- Label generators: `firmware`, `family`, `driver-version`, `driver-src-version`, `device-id`, `product-name`, `vram`, `simd-count`, `cu-count`, `compute-memory-partition`, `compute-partitioning-supported`, `memory-partitioning-supported`.
- A boolean flag of the same name switches each one on.
- With one distinct value, the label is `amd.com/gpu.<property>=<value>`.
- With several values, there is one label `amd.com/gpu.<property>.<value>=<count>` per value (`createLabels`).
- `firmware` labels exist only under `beta.amd.com`.

When labels are computed:

- The labels are computed once at start-up.
- The watch predicate passes only Create events for its own node. It ignores updates, deletes and generic events.
- So, as far as the code shows, a changed GPU configuration needs a labeller restart.
- Before writing, the labeller deletes every label it manages (bare and dotted variants). So stale values disappear (`removeOldNodeLabels`).

Note: the repo's `k8s-ds-amdgpu-labeller.yaml` deploys the upstream image `rocm/k8s-device-plugin:labeller-latest`. It is not an image built from this repo. `cmd/k8s-node-labeller/README.md` is upstream text. It includes a label example with an older `kubeadm` annotation. Its family list matches `FamilyIDtoString` in `internal/pkg/amdgpu/amdgpu.go`.

---

## 3. Resource names and how each is enforced

| Resource | Registered with kubelet by this plugin? | Meaning | Enforced by |
|---|---|---|---|
| `amd.com/gpu` | Yes. `AMDGPULister.GetResourceNamespace()` is `amd.com`. The resource last name is `gpu` under the default naming strategy. | Number of GPUs for the container. Kubelet sees one device per share. So a node with N GPUs and share count S shows `N x S` allocatable. | Placement: HAMi `AMDDevices.Fit` (`dev.Count <= dev.Used` fails with `CardTimeSlicingExhausted`). Devices: the plugin's `Allocate` (device nodes or CDI). |
| `amd.com/gpucores` | No (HAMi request resource). Optional node capacity with `-report_node_capacity`. | Percent of each allocated GPU's CUs. It is an integer from 1 to 100. The webhook rejects anything else (`MutateAdmission` in HAMi). Omitted means 100 in `GenerateResourceRequests`. | The scheduler turns the percent into a CU count and books it. The count is `Totalcore * pct / 100`, at least 1, rounded up to `cuPerWGP` multiples, capped at the GPU's CUs. The plugin turns the count into a bitmap and `HSA_CU_MASK`. The ROCm runtime applies the mask. `libamvgpu` pins it. This is cooperative, not a security boundary. |
| `amd.com/gpumem` | No (HAMi request resource). Optional node capacity with `-report_node_capacity`. | VRAM per GPU in MiB. Omitted means the whole VRAM. | The scheduler books it. The plugin sets `HIP_DEVICE_MEMORY_LIMIT_<i>=<MiB>m` (enforced in-process by `libamvgpu`). Where usable, it also writes `dmem.max` (enforced by the kernel). |
| `amd.com/gpumem-percentage` | No | A HAMi-side resource name (`devices.amd.resourceMemoryPercentageName` in the HAMi chart). It is a share of the GPU's memory, an integer from 0 to 100. A percentage alone makes the webhook add `amd.com/gpu: 1`. | HAMi converts it to MiB using the published `devmem` (`Fit`). This repo's README does not mention it. |
| Mixed-strategy names such as `amd.com/cpx_nps4` | Yes, with `-resource_naming_strategy=mixed` | Partitioned Instinct GPUs, named after the `<compute>_<memory>` type (`resourceNames` in `cmd/k8s-device-plugin/main.go`). | Same as `amd.com/gpu`. |

Details worth knowing:

- HAMi's chart lists `amd.com/gpu`, `amd.com/gpumem`, `amd.com/gpucores` (and the percentage name) as resources managed by the scheduler extender. They have `ignoredByScheduler: true` (HAMi `charts/hami/templates/_helpers.tpl`, "AMD resources"). So kube-scheduler does not try to fit `gpumem` or `gpucores` against node capacity.
- Default resource names in HAMi's chart (`charts/hami/values.yaml`, `devices.amd`): `amd.com/gpu`, `amd.com/gpumem`, `amd.com/gpumem-percentage`, `amd.com/gpucores`. HAMi's built-in default config (`pkg/scheduler/config/config.go`) names only `amd.com/gpu`.
- A request without `amd.com/gpucores` is a whole-GPU-CU request from HAMi's point of view. The core percentage defaults to 100.
- The plugin decides "whole GPU" in `isWholeGPU`. Every device must have cores >= the published `devcore` (or 0). It must also have memory >= the published `devmem` (or 0).
- Only non-whole requests get `LD_AUDIT`, `HIP_DEVICE_MEMORY_LIMIT_<i>` and the hook mount.
- Whole-GPU pods still get `HSA_CU_MASK` covering every CU, `ROCR_VISIBLE_DEVICES` and `HIP_VISIBLE_DEVICES`. But their CU bitmap is not saved (no `hami.io/amd-cu-allocated`). They also get no dmem cap.
- Do not put whole-GPU pods and slices on the same card when VRAM isolation matters. See "dmem VRAM cap" in `docs/user-guide/configuration.md`. The header of `internal/pkg/dmem/dmem.go` refers to Project-HAMi/HAMi#3144.
- A slice that names zero cores but less than the whole memory (`Usedcores == 0`, not whole) is a "memory-only slice". Its mask is `i:0-<total-1>` and it holds no CU bits (`TestAllocateMemoryOnlySliceHoldsNoCUs`).
- Note: a request that really reaches the plugin with only `amd.com/gpumem` never has `Usedcores == 0`. The HAMi scheduler books 100 percent of the cores when `amd.com/gpucores` is omitted.
  - Observed on an MI325X: such a Pod gets `HSA_CU_MASK=0:0-303` and holds all 304 CUs.
  - So only one runs at a time. The others wait with `CardInsufficientCore`.
  - HAMi pull request #3192 (open) adds `defaultCores` and `defaultMemory` settings to change that.
- On RDNA, the core count is rounded up to whole WGPs, first-fit from CU 0 (`cuallocation.AllocateN`, `unit = cuPerWGP`). Example from `docs/user-guide/resource-allocation.md`: 3 CUs become 4 (`0:0-3`). The next slice starts at `0:4`.
- Optional pod annotations that the HAMi AMD scheduler honours (`pkg/device/amd/device.go`):
  - `amd.com/use-gputype` and `amd.com/nouse-gputype`: substring match, case-insensitive, against the registered `type`.
  - `amd.com/use-gpu-uuid` and `amd.com/nouse-gpu-uuid`.
  - `amd.com/numa-bind`: all GPUs of a request go on one NUMA node.
  - The constants `amd.com/gpu-index` and `amd.com/predicate-node` exist in that file. But they are not used elsewhere in HAMi's non-test code.

---

## 4. Repo layout

```
.
|-- cmd/
|   |-- k8s-device-plugin/      main.go, main_test.go
|   |-- k8s-node-labeller/      main.go, controller.go, main_test.go, README.md
|   `-- k8s-vgpu-monitor/       main.go, main_test.go
|-- internal/pkg/
|   |-- allocator/              allocator.go, besteffort_policy.go, device.go (+ tests)
|   |-- amdgpu/                 amdgpu.go (+ test)
|   |-- amdsmi/                 amdsmi.go (+ test)
|   |-- cuallocation/           cuallocation.go (+ test)
|   |-- dmem/                   dmem.go (+ test)
|   |-- exporter/               health.go (+ test), metricssvc/ (generated gRPC client)
|   |-- hwloc/                  hwloc.go
|   |-- libcheck/               libcheck.go (+ test)
|   |-- plugin/                 plugin.go, cdi.go, health_events.go, xgmi.go (+ tests, fuzz test)
|   |-- utils/                  client.go, nodelocks.go, options.go, types.go, utils.go (+ test)
|   `-- vgpumonitor/            cgroup.go, collector.go (+ tests)
|-- helm/amd-gpu/               Chart.yaml, values.yaml, README.md, templates/
|-- scripts/amd-vgpu-init.sh    hook installer used by the chart's postStart hook
|-- testdata/                   fixtures (section 9.3)
|-- example/                    pod and deployment examples
|-- docs/                       Sphinx docs (user-guide/, contributing/, conf.py, sphinx/)
|-- amd-hami-core/              git submodule (empty until `git submodule update --init`)
|-- Dockerfile, ubi-dp.Dockerfile, labeller.Dockerfile, ubi-labeller.Dockerfile
|-- Makefile                    image build and save targets
|-- k8s-ds-amdgpu-labeller.yaml example labeller manifest (RBAC + DaemonSet)
|-- .github/                    workflows, issue templates, PR template, dependabot
|-- CHANGELOG.md, CONTRIBUTING.md, CODE_OF_CONDUCT.md, SECURITY.md, SECURITY-INSIGHTS.yml, LICENSE
`-- .readthedocs.yaml           Read the Docs build of docs/
```

There is no `OWNERS` file in this repo. There is no `vendor/`. CI does not build the top-level `docs/`.

### 4.1 `cmd/`

| Package | What is in it |
|---|---|
| `cmd/k8s-device-plugin` | `main.go` parses flags and validates them (`-split_count` not negative, `-allocator_policy` known, `-resource_naming_strategy` single or mixed). It calls `amdgpu.SetExcluded` and builds `plugin.AMDGPULister`. It starts the `dpm` manager and the `-pulse` heartbeat goroutine. It also starts a goroutine that runs only if `/sys/class/kfd` exists. That goroutine computes resource names and sends them to the manager. If there are none, it calls `plugin.ClearRegistration`. `main.go` also holds `ResourceNamingStrategy` and `resourceNames`. `main_test.go` covers `resourceNames`. |
| `cmd/k8s-vgpu-monitor` | `main.go` has the flags, the in-cluster client, a pod informer limited to the node (`spec.nodeName`), a Prometheus registry with `vgpumonitor.Collector`, and an HTTP server with graceful shutdown. It needs env `NODE_NAME`. `main_test.go` covers start-up errors, serving and the AMD collector. |
| `cmd/k8s-node-labeller` | `main.go` has the label generators, the flags, and the manager and controller set-up. `controller.go` has `reconcileNodeLabels`. There are also `README.md` and `main_test.go`. It needs env `DS_NODE_NAME`. |

### 4.2 `internal/pkg/`

| Package | Responsibility |
|---|---|
| `allocator` | `GetPreferredAllocation` policies. The `Policy` interface has `Init` and `Allocate`. `NewPolicy` gives the best-effort policy for `besteffort`, empty or `binpack`, and the spread variant for `spread`. `besteffort_policy.go` does subset selection. `device.go` has pair weights from the KFD topology, partition grouping and candidate subsets. |
| `amdgpu` | sysfs and KFD topology discovery (`GetAMDGPUs`, `GetDevIdsFromTopology`, `GetNodeIdsFromTopology`, `GetROCrUUIDsFromTopology`, `GetROCrIndexesFromTopology`, `ParseTopologyProperties`). Partition sysfs reads. The exclusion list. libdrm queries through cgo: capacity, family, firmware, marketing name, and the open-the-device check `DevFunctional`. |
| `amdsmi` | AMD SMI cgo wrapper: UUIDs, product names, current memory partitions, partition profiles, and setting the compute partition. It has a process-wide mutex, a cache for static lookups and `normalizeBDF`. |
| `cuallocation` | Segmented 64-bit CU bitmap: `NewAllocation`, `AllocateN` (first-fit, in whole units), `AddAllocation`, `CountAllocated`. |
| `dmem` | Kernel dmem cgroup helper: `Available`, `Usable`, `Region`, `PodCgroupPath`, `SetMax`, `SetMaxWithRetry`, `NormalizeBDF`. |
| `exporter` | Client of the AMD Device Metrics Exporter health gRPC socket (`getGPUHealth`, `PopulatePerGPUDHealth`, `UnhealthyGPUs`). Also `metricssvc/`, which holds generated `.pb.go` files copied from the exporter project. Only `List` is used. See its `doc.go`. |
| `hwloc` | `GetVersions()` logs the hwloc version at start-up (cgo, pkg-config `hwloc`). That is its only use. |
| `libcheck` | Looks inside a container image's root filesystem (mounted read-only through `ctr images mount`). It classifies the image as glibc, old glibc, musl or static (`Inspector.Inspect`). It is used by `-musl_fail_closed`. |
| `plugin` | The device plugin. `plugin.go` holds everything in section 2. `cdi.go` writes the CDI spec and names. `health_events.go` has node Events and the mode-change warning. `xgmi.go` has `xgmiPeers` and `readLink`. |
| `utils` | Kubernetes client bootstrap (`KUBECONFIG`, else `~/.kube/config`, else in-cluster). Node helpers (`GetNode`, `PatchNodeAnnotations`, `PatchNodeStatusCapacity`, `PatchPodAnnotations`). Pending-pod lookup. Pod allocation success and failure handling. Node lock parse and release. Annotation encode and decode types. Its files carry "The HAMi Authors" copyright headers. |
| `vgpumonitor` | `cgroup.go` walks `kubepods.slice` and reads `dmem.current` and `dmem.max` of container scopes. `collector.go` is the Prometheus collector. |

---

## 5. Flags of every binary

All three binaries import `github.com/golang/glog`. So each also has the glog flags (from `glog@v1.2.5`):

- `-v` (default 0)
- `-vmodule`
- `-log_backtrace_at`
- `-logtostderr` (false)
- `-alsologtostderr` (false)
- `-stderrthreshold` (default ERROR)
- `-log_dir` (empty)
- `-log_link` (empty)

The images pass `-logtostderr=true -stderrthreshold=INFO -v=5` (Dockerfile `CMD`). The chart passes its own (section 6.2).

### 5.1 `k8s-device-plugin` (`cmd/k8s-device-plugin/main.go`)

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-pulse` | int | `0` | Seconds between health checks (heartbeats to `ListAndWatch`). 0 disables them. The chart sets 10. |
| `-resource_naming_strategy` | string | `single` | `single` or `mixed`. See section 5.1.1. The chart does not pass it. |
| `-split_count` | int | `0` | How many workloads may share one GPU (HAMi device `Count`). 0 picks per GPU: 2 on gfx12, else 10. A negative value logs an error and exits with status 1. |
| `-report_node_capacity` | bool | `false` | Publish the healthy GPUs' `amd.com/gpumem` (MiB) and `amd.com/gpucores` as node capacity and allocatable. With the flag off, the plugin removes entries that a previous run left. It needs RBAC `nodes/status patch`, which the chart always grants. |
| `-hip_log_level` | int | `0` | `LIBHIP_LOG_LEVEL` for pods that load libamvgpu. 0 keeps the library default. |
| `-exclude_gpus` | string | empty | Comma-separated PCI BDFs (`<bdf>`) or DRM cards (`card1`) that the plugin leaves out. Case-insensitive and trimmed. |
| `-device_memory_scaling` | float | `1` | Multiplies the VRAM that each GPU registers. Above 1, pods get `HIP_OVERSUBSCRIBE=true`. A value of 0 or below is ignored (it stays 1). |
| `-cdi_spec_dir` | string | empty | Write a CDI spec for `amd.com/gpu` here and inject devices through CDI. Empty uses device nodes. |
| `-allocator_policy` | string | `besteffort` | `besteffort`, `binpack` (same as besteffort) or `spread`. Anything else exits with an error. |
| `-dmem_backend` | bool | `true` | Also cap sliced allocations through the dmem cgroup controller. It works only if `dmem.Usable` finds the controller and `kubepods.slice`. |
| `-musl_fail_closed` | bool | `false` | Refuse a sliced allocation whose image cannot load the hook. Experimental. `Allocate` runs at pod admission, before kubelet pulls the image. So an image not yet on the node is pulled in the background, and the first Pod is rejected with `image is not on the node yet`. The next Pod starts. |
| `-ctr_path` | string | `/usr/local/bin/ctr` | The `ctr` binary inside the plugin pod. Used only with `-musl_fail_closed`. The chart passes `/opt/amd-gpu-ctr-host/ctr`. |
| `-containerd_socket` | string | `/run/containerd/containerd.sock` | The containerd socket inside the plugin pod. Used only with `-musl_fail_closed`. |

Environment variables read by the plugin:

- `NODE_NAME` (required; `utils.NodeNameEnvName`).
- `OPERATING_MODE` (default `cu`).
- `COMPUTE_PARTITION` (empty).
- `HOST_HOOK_PATH` (the host prefix of the hook, set by the chart).
- `KUBECONFIG` and `HOME` (kubeconfig lookup; in-cluster config is the fallback).
- `HAMI_NODELOCK_EXPIRE` is parsed but unused here (see section 2.2).
- The hook installer script reads `AMD_VGPU_HOOK_SOURCE` (default `/opt/hami/lib/amd/libamvgpu.so`).

#### 5.1.1 Naming strategy

`resourceNames` looks at the distinct `<compute>_<memory>` partition types across GPUs. It uses `amdgpu.UniquePartitionConfigCount`. Only GPUs that report both types count.

- Homogeneous node (zero or one distinct type), `single`: the resource is `gpu`.
- Homogeneous node, `mixed`: the resource is the partition type name (for example `spx_nps1`). If no GPU reports partition types (RDNA, VFs), it is `gpu`.
- Heterogeneous node, `single`: it logs a warning and registers every device under `gpu`.
- Heterogeneous node, `mixed`: one resource per partition type with a positive count. In this branch, a GPU without partition types matches no name.

Note: `docs/user-guide/configuration.md` says that under `mixed`, "unpartitioned GPUs stay `amd.com/gpu`". The code does that only when no GPU of the node reports partition types. See `resourceNames`, and `kubeletDevices` and `devicesForResource` in `plugin.go`.

### 5.2 `k8s-vgpu-monitor` (`cmd/k8s-vgpu-monitor/main.go`)

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `-metrics_bind_address` | string | `:9394` | TCP address for `/metrics`. The chart passes `monitor.metricsBindAddress`. |
| `-cgroup_root` | string | `/sys/fs/cgroup` | The cgroup v2 mount of the host. |
| `-drm_root` | string | `/sys/class/drm` | The sysfs DRM class directory of the host. |

- Env: `NODE_NAME` (required).
- It must run in-cluster (`rest.InClusterConfig`).
- It binds the port before it starts the informer. So a taken port fails fast.

### 5.3 `k8s-node-labeller` (`cmd/k8s-node-labeller/main.go`)

- There are twelve boolean flags. All default to `false`. Each means "label nodes with this property".
  - `-firmware`, `-family`, `-driver-version`, `-driver-src-version`, `-device-id`, `-product-name`, `-vram`, `-simd-count`, `-cu-count`, `-compute-memory-partition`, `-compute-partitioning-supported`, `-memory-partitioning-supported`.
- The controller-runtime config package also registers `-kubeconfig` (default empty; in-cluster otherwise).
- Env: `DS_NODE_NAME` (required). The process exits with an error if it is unset (`ownNodeName`).
- The example manifest enables `-vram -cu-count -simd-count -device-id -family -product-name`.

---

## 6. Helm chart `helm/amd-gpu`

`Chart.yaml`: name `amd-gpu`, `version` `0.0.3`, `appVersion` `"0.0.3"`, `kubeVersion` `">= 1.19.0-0"`.

Install:

```
helm upgrade --install amd-gpu ./helm/amd-gpu --namespace kube-system --create-namespace
```

A chart-releaser workflow publishes the chart when `helm/**` changes on main (section 9.4).

### 6.1 Every value (`values.yaml`)

| Key | Default | What it does in the templates |
|---|---|---|
| `dp.image.repository` | `ghcr.io/project-hami/amd-device-plugin` | Image repository for both DaemonSets. The monitor reuses it. |
| `dp.image.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `dp.image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `dp.serviceAccount.create` | `true` | Creates the ServiceAccount, the ClusterRole and the binding. |
| `dp.serviceAccount.name` | `""` | Name override. With `create: false`, RBAC binds to this existing account. It is skipped when the name is empty (`rbac.yaml`: "Never bind the namespace default service account"). |
| `dp.serviceAccount.annotations` | `{}` | ServiceAccount annotations. |
| `dp.operatingMode` | `cu` | Env `OPERATING_MODE`. `cu` or `partition`. The node annotation `hami.io/amd-operating-mode` overrides it. |
| `dp.computePartition` | `""` | Env `COMPUTE_PARTITION`. Used only when the mode is `partition`. The node annotation `hami.io/amd-compute-partition` overrides it. |
| `dp.securityContext.privileged` | `true` | Plugin container security context (rendered with `toYaml`). |
| `dp.securityContext.capabilities.drop` | `[ALL]` | Capabilities to drop. |
| `dp.hostHookPath` | `/usr/local` | Env `HOST_HOOK_PATH`. The hook lives at `<hostHookPath>/vgpu/libamvgpu.so` on the host. Inside workloads it is always `/usr/local/vgpu/libamvgpu.so` (hard-coded in `Allocate`). |
| `dp.excludeGPUs` | `[]` | Joined with commas into `-exclude_gpus`. |
| `dp.hipLogLevel` | `0` | `-hip_log_level`. Passed only when non-zero. |
| `dp.logVerbosity` | `2` | `-v` of both the plugin and the monitor. |
| `dp.healthPulse` | `10` | `-pulse`. |
| `dp.splitCount` | `0` | `-split_count`. |
| `dp.reportNodeCapacity` | `false` | `-report_node_capacity=true` when set. The ClusterRole always has `nodes/status patch`. So the plugin can also remove the entries after the option is turned off. |
| `dp.deviceMemoryScaling` | `1` | `-device_memory_scaling`. Passed only when it differs from 1. |
| `dp.allocatorPolicy` | `besteffort` | `-allocator_policy`. |
| `dp.dmemBackend` | `true` | `-dmem_backend`. |
| `dp.muslFailClosed.enabled` | `false` | Adds `-musl_fail_closed=true`, `-ctr_path=/opt/amd-gpu-ctr-host/ctr`, `-containerd_socket=<containerdSocket>` and three volumes. |
| `dp.muslFailClosed.ctrPath` | `/var/lib/rancher/rke2/bin/ctr` | Host path of `ctr`, mounted read-only at `/opt/amd-gpu-ctr-host/ctr`. The defaults are RKE2 paths. On k3s use `/var/lib/rancher/k3s/data/current/bin/ctr`. |
| `dp.muslFailClosed.containerdSocketDir` | `/run/k3s/containerd` | Host directory of the socket, mounted with `mountPropagation: HostToContainer`. |
| `dp.muslFailClosed.containerdSocket` | `/run/k3s/containerd/containerd.sock` | Passed to `-containerd_socket`. |
| `dp.muslFailClosed.containerdDataDir` | `/var/lib/rancher/rke2/agent/containerd` | Snapshotter data directory, mounted with `HostToContainer`. On k3s use `/var/lib/rancher/k3s/agent/containerd`. |
| `dp.cdi.enabled` | `false` | Adds `-cdi_spec_dir` and a hostPath mount of the spec directory. |
| `dp.cdi.specDir` | `/var/run/cdi` | Host and container path of the CDI spec directory. |
| `dp.hookInstaller.enabled` | `true` | Adds the `postStart` lifecycle hook that runs `/opt/hami/bin/amd-vgpu-init.sh <hostHookPath>/vgpu`. Also adds a hostPath volume `host-vgpu-hook` (type `DirectoryOrCreate`). |
| `dp.resources` | `{}` | Plugin container resources. |
| `dp.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | DaemonSet update strategy. |
| `monitor.enabled` | `false` | Renders the monitor DaemonSet. Also adds `watch` on pods to the ClusterRole. |
| `monitor.metricsBindAddress` | `:9394` | `-metrics_bind_address`. The container port and the `prometheus.io/port` annotation come from it. |
| `monitor.resources` | `{}` | Monitor container resources. |
| `imagePullSecrets` | `[]` | Applies to both DaemonSets. |
| `tolerations` | `[{key: CriticalAddonsOnly, operator: Exists}]` | Applies to both DaemonSets. |
| `node_selector_enabled` | `false` | When true, apply `node_selector` to both DaemonSets. |
| `node_selector` | `feature.node.kubernetes.io/pci-0300_1002.present: "true"`, `kubernetes.io/arch: amd64` | Used only when enabled. |

Not in `values.yaml` but read by `templates/_helpers.tpl`: `nameOverride` and `fullnameOverride`.

Note: `helm/amd-gpu/README.md` did not list `dp.operatingMode` or `dp.computePartition` when this guide was written. `values.yaml` and `docs/user-guide/configuration.md` did.

### 6.2 What the templates create

- Device plugin DaemonSet `amd-gpu-device-plugin-daemonset` (named from `.Chart.Name`).
  - Selector label `name=amd-gpu-dp-ds`.
  - `priorityClassName: system-node-critical`.
  - Container `amd-gpu-dp-cntr`, command `./k8s-device-plugin`.
  - Args: `-logtostderr=true -stderrthreshold=INFO -v=<logVerbosity> -pulse=<healthPulse> -split_count=<splitCount> -allocator_policy=<allocatorPolicy> -dmem_backend=<dmemBackend>`, plus the conditional ones above.
  - Env: `NODE_NAME` (downward API `spec.nodeName`), `HOST_HOOK_PATH`, `OPERATING_MODE`, `COMPUTE_PARTITION`.
- Host mounts of the plugin:
  - `/var/lib/kubelet/device-plugins`.
  - `/sys`.
  - `/var/lib/amd-metrics-exporter/`, where the AMD Device Metrics Exporter's socket lives.
  - The conditional CDI, hook and containerd mounts.
  - `/dev` is not mounted. The privileged context provides device access.
- Monitor DaemonSet `amd-gpu-monitor-daemonset` (only if enabled).
  - Command `./k8s-vgpu-monitor`. It shares the plugin's ServiceAccount.
  - Read-only host mounts of `/sys/fs/cgroup`, `/sys/class/drm` and `/sys/devices`. The DRM `card*/device` links point into `/sys/devices`.
  - `readOnlyRootFilesystem`, not privileged, with liveness and readiness probes on `/metrics`.
- RBAC (`rbac.yaml`): ClusterRole `<fullname>-device-plugin` with these rules:
  - `nodes`: get, patch.
  - `nodes/status`: patch.
  - `events`: create.
  - `pods`: get, list, patch (plus `watch` with the monitor).
- Chart NOTES: a single line that names the DaemonSet and namespace.

---

## 7. Hardware topics

### 7.1 gfx generations, RDNA and CDNA

The plugin reads `gfx_target_version` from the KFD topology (`/sys/class/kfd/kfd/topology/nodes/<n>/properties`). KFD encodes it as `major*10000 + minor*100 + step`. For example:

- `90402` is gfx942.
- `90010` is gfx90a (step 10 is written `a`).
- `120001` would be gfx1201.

Only two thresholds exist in the code:

| Version test | Code | Effect |
|---|---|---|
| `>= 100000` (gfx10 and later, RDNA) | `cuPerWGP` | CU masks apply per WGP, so slices are whole WGPs (`custominfo.cuPerWGP = 2`). |
| `>= 120000` (gfx12, RDNA4) | `splitCountFor` | The default share count is 2 instead of 10. |

- Everything below 100000 (gfx9, CDNA and older Vega-class parts) uses single-CU masks and share count 10.
- gfx11 (RDNA3, versions 110000 to 119999) has no code path of its own. By the thresholds above it gets WGP slices and 10 sharers.
- `README.md` ("Tested hardware") says that RDNA3 and CDNA compute or memory slicing have not been validated.

Validated hardware per `README.md`:

- MI300X VF (registration only).
- Radeon RX 9060 XT (gfx1200).
- RX 9070 XT (gfx1201) plus a Radeon iGPU (gfx1036).

The verification pass in section 11.1 adds an MI325X VF (gfx942, 304 CUs, 256 GB). It covered registration, CU slicing, the hook, dmem caps on kernel 7.0, the monitor, the labeller and the exporter health paths.

The MI355X (gfx950) is documented in `docs/user-guide/mi355x-partitioning.md`. It is tested through captured fixtures, not with live GPU tests in CI.

Family labels (labeller) come from libdrm family IDs. The code defines fallback values for `GC_11_0_0` (145), `GC_11_0_1` (148), `GC_10_3_6` (149), `GC_10_3_7` (151), `GC_11_5_0` (150) and `GC_12_0_0` (152). So older libdrm headers still recognise these GPUs (cgo preamble of `amdgpu.go`).

gfx12 sharing:

- `README.md` ("Limits") and `docs/user-guide/resource-allocation.md` explain why the default is 2.
- gfx12 has few user compute queues. So more than about 2 processes per GPU can lose most of their throughput.
- `custominfo.computeQueues` publishes the KFD `num_cp_queues`.

### 7.2 WGP versus CU

- A CU (compute unit) is the unit that `amd.com/gpucores` and `devcore` count.
- On RDNA, the hardware groups two CUs into a WGP (workgroup processor).
- `HSA_CU_MASK` entries that enable only one CU of a pair are ignored by the GPU. The pod would then run on the whole GPU. See "Compute units" in `docs/user-guide/resource-allocation.md` and the `cuMaskUnit` comment in `plugin.go`.
- So the plugin hands out WGPs and rounds up.
- A single-WGP GPU, such as a small iGPU, can only be given out whole.

### 7.3 Compute partitions SPX, DPX, QPX, CPX and memory partitions NPS

This applies to Instinct GPUs that expose `current_compute_partition` and `current_memory_partition` files in sysfs (CDNA). RDNA, SR-IOV VFs and embedded APUs do not. They stay CU-maskable in both modes.

MI355X-class (8 XCCs) profiles, as reported by `amd-smi partition -g <id> --json`. Sources: `README.md`, `testdata/amdsmi-partition-g3-mi355x.json`, asserted by `TestPartitionProfilesFromFixture`.

| Profile index | Type | Memory modes allowed | Partitions x XCC per partition | CUs per partition on MI355X (256 total, `docs/user-guide/mi355x-partitioning.md`) |
|---|---|---|---|---|
| 0 | SPX (default) | NPS1 | 1 x 8 | 256 |
| 1 | DPX | NPS1, NPS2 | 2 x 4 | 128 |
| 2 | QPX | NPS1 | 4 x 2 | 64 |
| 3 | CPX | NPS1 | 8 x 1 | 32 |

`IsPartitionMode` and the AMD SMI type mapping also know `tpx`. The MI355X fixture has no TPX profile.

Operating modes are chosen per node. The precedence is: default `cu`, then env `OPERATING_MODE`, then the node annotation `hami.io/amd-operating-mode` (`Start()` in `plugin.go`). Unknown values log a warning and use `cu`.

- `cu` (default):
  - Every whole GPU registers as a soft device with `Count` set to the share count.
  - Kubelet sees `<key>#0 ... <key>#<count-1>`.
  - XCP partition devices are not registered.
- `partition`:
  - Every compute partition registers as a hard device (`Count` 1, `Mode` set to the type).
  - Kubelet sees one `<key>#<type>` per hard device.
  - XCP partitions (`amdgpu_xcp_*` platform devices) replace their parent GPU.
  - A partitioned GPU whose kernel exposes no XCP nodes registers as one hard whole-GPU device.
  - GPUs without a partition type stay soft.
- With `COMPUTE_PARTITION` (or the node annotation) set in partition mode, `Start()` flips GPUs that are not yet in that type before it registers. It uses `amdsmi_set_gpu_accelerator_partition_profile`. A busy GPU fails to flip and keeps its mode. The plugin logs it and continues.
- XCP entries share the parent's BDF. They get floor(VRAM / n) and floor(CUs / n).
  - Here n is `NumPartitions` of the current profile from AMD SMI.
  - It is not the count of `amdgpu_xcp_*` children. That count is wrong on gfx950, which shows 7 XCD devices per GPU in every mode. See `docs/user-guide/mi355x-partitioning.md`.
- Changing the operating mode while pods run emits the `AMDGPUModeChangedInUse` Event and a log warning. Drain the node first (`warnModeChangeInUse`).

Memory partitions (NPS1, NPS2, NPS4, NPS8):

- They are a NUMA-interleaving setting of the GPU's memory.
- They are fixed at boot. Only an amdgpu driver reload changes them, and that reload resets the device nodes of every GPU. See "Memory partitions are not an allocation mechanism" in `README.md`.
- The plugin reads the current NPS value (AMD SMI, falling back to sysfs).
- It uses the value only to build the `<compute>_<memory>` partition type for resource naming and `partitionProfile`.
- It never changes the value. The per-pod memory limit does not depend on it.

Kernel dependency:

- Whether `amdgpu_xcp_*` partitions register depends on the kernel's KFD topology exposing XCP render minors.
- On kernel 6.8.0-136 it did not. So 8 whole-GPU entries registered.
- The fixture `testdata/sysfs-mi355x-spx` captures that case (`docs/user-guide/mi355x-partitioning.md`).

What is verified and what still needs a bare metal machine is in section 11.0.

### 7.4 XGMI peers

- KFD link type 11 is XGMI (Infinity Fabric between GPUs). Type 2 is PCIe. See `kfdXGMILinkType` in `xgmi.go` and `calculatePairWeight` in `device.go`.
- `xgmiPeers` scans a GPU node's `io_links/*/properties` and `p2p_links/*/properties`.
- It keeps links of type 11 whose `node_to` is another registered GPU with a different BDF.
- It returns the sorted BDFs. The result is `custominfo.xgmiPeers`.
- Partitions of one GPU share its BDF. So they list the other GPUs and not each other.

The allocator uses the same link types for `GetPreferredAllocation` weights (`internal/pkg/allocator/device.go`):

| Component | Weight |
|---|---|
| Same physical GPU (same `DevId`) | 10 |
| Different GPU | 20 |
| Link XGMI | +10 |
| Link PCIe | +40 |
| Any other link, or no direct link (treated as through the host) | +50 |
| Same NUMA node | +10 |
| Different NUMA node | +20 |

- `besteffort` and `binpack` choose the subset with the lowest total weight. `spread` chooses the highest.
- Partitions of one GPU are grouped and filled first. This limits fragmentation.
- If the allocator cannot map kubelet's ids (for example a second plugin owns some of them), `GetPreferredAllocation` echoes kubelet's own ids.
- If `Init` fails, `GetDevicePluginOptions` disables the preferred-allocation option.

### 7.5 Health checks

1. Kubelet-facing, every `-pulse` seconds (heartbeat in `ListAndWatch` and `serveDevices`).
   - First, `exporter.PopulatePerGPUDHealth` reads the exporter state (for example ECC or throttling). It uses the gRPC socket `/var/lib/amd-metrics-exporter/amdgpu_device_metrics_exporter_grpc.socket` with a 5 s timeout.
   - Then `markNonFunctional` marks every share of a GPU unhealthy when libdrm cannot open its DRM card (`amdgpu.DevFunctional`).
   - Without the exporter, only the open check applies. An unreachable exporter is logged once per outage.
2. Scheduler-facing, every registration (30 s).
   - `health` in the annotation is false when the exporter reports the GPU unhealthy.
   - HAMi's `Fit` then skips it. Newer schedulers say `CardNotHealth`. HAMi v2.10.0 reports the node as `unregistered`.
   - A GPU whose capacity cannot be read is dropped from the annotation.
3. HAMi node-level: `AMDDevices.CheckHealth` reads the node's `status.allocatable["amd.com/gpu"]`.
   - It does not read capacity, because kubelet drops unhealthy devices from allocatable.
   - Zero means unhealthy. The scheduler then cleans up that node's AMD devices.
4. Events: state transitions become node Events (section 2.3).

### 7.6 XNACK

- XNACK is the GPU's ability to retry memory accesses after page faults. Managed-memory oversubscription depends on it.
- In this repo it appears in one place only. The chart README says `dp.deviceMemoryScaling` oversubscription "needs XNACK on CDNA".
- No code in the plugin, the chart or amd-hami-core sets or checks it. Enabling it on the node or in the workload is the user's job.
- Oversubscription works like this:
  - The plugin multiplies the registered `devmem`.
  - It sets `HIP_OVERSUBSCRIBE=true`.
  - `libamvgpu` serves `hipMalloc` from `hipMallocManaged` and still applies the memory limit.

---

## 8. Building

### 8.1 Images (`Makefile`)

`IMAGE_REPO` defaults to `ghcr.io/project-hami/amd-device-plugin`. `IMAGE_VERSION` defaults to `0.0.3`.

| Target | Dockerfile | Tag | Base |
|---|---|---|---|
| `build-device-plugin` | `Dockerfile` | `<version>` | runtime `rocm/dev-ubuntu-24.04:7.2.4` (digest pinned) |
| `build-labeller` | `labeller.Dockerfile` | `labeller-<version>` | `alpine:3.24.2` (digest pinned), builder `golang:1.27-alpine` |
| `build-ubi-device-plugin` | `ubi-dp.Dockerfile` | `rhubi-<version>` | `ubi9/ubi-init:9.8` |
| `build-ubi-labeller` | `ubi-labeller.Dockerfile` | `labeller-rhubi-<version>` | `ubi9/ubi-minimal:9.8` |

- `make all` builds and saves everything.
- The `save-*` targets run `docker save | gzip` into `dist/tarballs/` (`TAR_DIR`, `OUTPUT_DIR`, git-ignored).
- `make clean` removes images and `dist/`.
- `make help` prints the list.

The main `Dockerfile` has these stages:

1. `rocm-runtime`: `rocm/dev-ubuntu-24.04:7.2.4`. This is the final base.
2. `amdsmi-sdk`: `rocm/dev-ubuntu-22.04:7.2.4`.
   - It installs cmake and build-essential.
   - It builds `amd-hami-core` (`make -f Makefile.hip clean all`) from the submodule directory. So `libamvgpu.so` has an older glibc baseline.
   - It is also the source of the AMD SMI headers and library.
3. `builder`: `golang:1.26`.
   - It installs `git pkg-config build-essential libdrm-dev libhwloc-dev`.
   - It copies AMD SMI headers and `libamd_smi.so` into `/opt/rocm/...`. It uses libstdc++ from the 22.04 image, so it has an older baseline than the 24.04 runtime.
   - It builds `k8s-device-plugin` with `-X main.gitDescribe=<git describe>`.
   - It builds `k8s-vgpu-monitor` with `CGO_ENABLED=0`.
4. Final stage:
   - It installs `libdrm2 libhwloc15`.
   - It copies `libamd_smi.so` to `/opt/rocm-7.2.4/lib/libamd_smi.so.26.2.2`. The SONAME that the executable records is `libamd_smi.so.26`.
   - It copies the two binaries to `/root/`, `libamvgpu.so` to `/opt/hami/lib/amd/libamvgpu.so`, and `scripts/amd-vgpu-init.sh` to `/opt/hami/bin/` (mode 0555).
   - It asserts that the library file is non-empty.
   - `CMD ["./k8s-device-plugin", "-logtostderr=true", "-stderrthreshold=INFO", "-v=5"]`.

Other images:

- The UBI plugin image builds Go 1.26.9 with a checksum check.
- It installs `amd-smi-lib` from the ROCm 7.2.4 EL9 repo.
- It builds hami-core in a `rocm/dev-ubuntu-22.04:7.2.4` stage.
- It does not include `k8s-vgpu-monitor`.
- The labeller images download `amdgpu.ids` from the freedesktop libdrm repository at build time. They append rows for the MI325X VF, MI100, Radeon Pro W6900X, W6800X and MI308X HF VF.

The submodule:

- `amd-hami-core` is a submodule (`.gitmodules`, URL `https://github.com/Project-HAMi/amd-hami-core.git`). It is pinned to commit `816eb5d`.
- A fresh clone has an empty `amd-hami-core/` directory. Run `git submodule update --init`.
- CI checks out with `submodules: true`. Dependabot has a `gitsubmodule` entry.

### 8.2 cgo dependencies

| Package using cgo | Needs |
|---|---|
| `internal/pkg/amdgpu` | pkg-config `libdrm libdrm_amdgpu` (headers `xf86drm.h`, `amdgpu.h`, `amdgpu_drm.h`). Used by the plugin and the labeller. |
| `internal/pkg/amdsmi` | `#cgo CFLAGS: -I/opt/rocm/include`, `#cgo LDFLAGS: -L/opt/rocm/lib -lamd_smi -lstdc++`. Header `amd_smi/amdsmi.h`. |
| `internal/pkg/hwloc` | pkg-config `hwloc`. |

- `k8s-vgpu-monitor` has no cgo.
- A local `go build ./...` needs `pkg-config`, `libdrm-dev`, `libhwloc-dev` and a ROCm install that provides `amd_smi` (`CONTRIBUTING.md`). The supported route is the builder image.
- The runtime needs `libamd_smi.so.26` to resolve. `README.md` and CI create the symlink `ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.26` with `LD_LIBRARY_PATH=/opt/rocm/lib` before they run tests.

### 8.3 amd-hami-core by itself

```
make -f Makefile.hip           # build-hip/libamvgpu.so (cmake + make, Release, -fvisibility=hidden, stripped)
make -f Makefile.hip test      # also builds build-hip/test_memory_limit with hipcc
docker build -f Dockerfile.hip --build-arg ROCM_IMAGE=<image> -t libamvgpu-builder .
docker run --rm -v $(pwd)/dist:/dist libamvgpu-builder   # dist/libamvgpu.so, dist/test_memory_limit
```

- `ROCM_HOME` defaults to `/opt/rocm`.
- `Dockerfile.hip` also runs `check_glibc_abi.sh`. It verifies that `la_version` and `la_symbind64` are exported.

### 8.4 Docs

- `docs/` is a Sphinx site that uses `rocm-docs-core` (`docs/sphinx/requirements.in`).
- Read the Docs builds it (`.readthedocs.yaml`, Python 3.12, Ubuntu 24.04).
- The table of contents is `docs/sphinx/_toc.yml.in`.
- CI does not build it.

---

## 9. Testing

### 9.1 Commands

Supported route: run inside the builder image (`README.md`, "Development"). CI does the same.

```
docker build --target builder -t amd-device-plugin-builder .
docker run --rm -e LD_LIBRARY_PATH=/opt/rocm/lib \
  --workdir /go/src/github.com/Project-HAMi/amd-device-plugin amd-device-plugin-builder \
  bash -c 'ln -sf libamd_smi.so /opt/rocm/lib/libamd_smi.so.26 && go test ./...'
helm lint helm/amd-gpu
```

- CI additionally runs `go vet ./...` and `go test -race ./...`.
- `CONTRIBUTING.md` asks for `go build ./...`, `go vet ./...` and `go test ./...`.

amd-hami-core:

- The unit tests need no GPU.
- CI uses `-Wall -Wextra -Werror -fsanitize=address,undefined`.
- `test_alloc_tracker test_env_policy test_memory_size test_queue_priority test_alloc_size test_oversubscribe` compile with `-I src/hip`.
- `test_reserve test_shrreg test_init test_retry` compile with `-pthread` together with `src/multiprocess/hip_multiprocess_memory_limit.c`.
- The threaded tests also run under ThreadSanitizer.
- `test/check_glibc_abi.sh dist/libamvgpu.so` checks the ABI.
- Note: the amd-hami-core README and CONTRIBUTING list fewer unit tests than CI runs.

HAMi side:

- `pkg/device/amd/device_test.go` and `device_alloctype_test.go` test the scheduler logic.
- `test/e2e/amd/` is the end-to-end suite.
- The workflow `e2e-amd-mock.yaml` runs it on a kind cluster with fake AMD GPUs.
- The e2e tests skip on clusters without a registered AMD GPU. `AMD_E2E_MOCK=true` selects the mock mode.

### 9.2 Which tests need a GPU, debugfs or the amdgpu driver

Everything else runs on any Linux machine with the cgo libraries (or in the builder image). A missing GPU makes these tests skip:

| Test (package) | Needs | Skips with |
|---|---|---|
| `TestFirmwareVersionConsistent` (`internal/pkg/amdgpu`) | An AMD GPU (vendor 0x1002 under `/sys/class/drm`) and debugfs `/sys/kernel/debug/dri/<n>/amdgpu_firmware_info` (usually root) | "no AMD GPU", "debugfs ... unavailable", or "DRM device not usable" |
| `TestAMDGPUcountConsistent`, `TestDevFunctional` (`amdgpu`) | An AMD GPU | "no AMD GPU found" |
| `TestPartitionLookupsWithoutPartitionSupport` (`internal/pkg/amdsmi`) | An AMD GPU without compute-partition support (it skips on partition-capable GPUs), and AMD SMI | "no AMD GPU", "GPU supports compute partitions" |
| `TestRocmModeRegistrationOnHardware` (`plugin`) | An AMD GPU | "no AMD GPU on this machine" |
| `TestListAndWatchOnHardware`, `TestStartOnHardware`, `TestStartWithoutNode` (`plugin`) | An AMD GPU (the Kubernetes API is a fake clientset) | "no AMD GPU" |
| `TestGetAPIDevicesSkipsGPUWithoutCapacity` (`plugin`) | Only `/sys/module/amdgpu/drivers/` (the amdgpu driver, no real device needed; libdrm is stubbed) | "no amdgpu driver" |
| `TestSetMaxWithRetryGivesUpEventually` (`dmem`) | Nothing, but it takes about 20 s | It skips with `go test -short` |

- The code that reads `/sys/module/amdgpu/drivers/` calls `glog.Fatalf` when the directory is absent (`GetAMDGPUs`). That is why tests that call it without a fixture root guard with `hasAMDGPU()` first.
- Changes to discovery, AMD SMI, ROCr visibility, CU masks or memory limits also need a run on a real GPU node. See "Development" in `README.md`. `amd-hami-core/CONTRIBUTING.md` asks the same for the audit library and the shared-region code.

Other kinds of tests:

- `FuzzIDListToAllocation` (`plugin/fuzz_test.go`) fuzzes the CU id-list parser and checks round trips. `go test` runs only its seed corpus unless you pass `-fuzz`.
- Tests with fake clients use `k8s.io/client-go/kubernetes/fake` and controller-runtime's fake client.
- `libcheck` tests build image-like directory trees in temp directories. They do not mount images.

### 9.3 Fixtures in `testdata/`

| Fixture | What it is | Used by |
|---|---|---|
| `sysfs-mi355x-spx/` | A captured `/sys` subset of an 8x MI355X node (gfx950, device id 0x75a3) in SPX/NPS1 on kernel 6.8.0-136. It came from a test node (details in its `README.md`). It holds trimmed `class/kfd/kfd/topology/nodes/*/properties`, `module/amdgpu/drivers/pci:amdgpu/<bdf>/` partition files and DRM entry names, and 56 `devices/platform/amdgpu_xcp_*` directories (7 per GPU). XCP render minors are absent from the KFD topology on this kernel, so discovery skips them. | `TestRegistrationFromFixture`, `TestRegistrationByROCrIndex`, `TestXCPRegistrationReplacesParent` (`plugin`). `TestGetAMDGPUsFromFixture` and the MI355X ROCr/devID tests (`amdgpu`). |
| `amdsmi-mi355x.json` | AMD SMI UUIDs and market names per BDF, injected as the AMD SMI stub. | `plugin` registration tests |
| `amdsmi-partition-m-mi355x.json` | Raw `amd-smi partition -m --json`: memory partition caps and the current setting per GPU. | `loadMemoryPartitionGolden` |
| `amdsmi-partition-g3-mi355x.json` | Raw `amd-smi partition -g 3 --json`: the four profiles (SPX, DPX, QPX, CPX). | `TestPartitionProfilesFromFixture`, `TestRegistrationFromFixture` |
| `amdsmi-partition-g0` to `g7-mi355x.json`, `amdsmi-partition-mi355x.json`, `amdsmi-static-mi355x.json`, `amdsmi-xgmi-mi355x.json`, `amdsmi-topology-mi355x.json` | More raw `amd-smi` JSON captures of the same node (per-GPU partition output, `static`, `xgmi`, `topology`). Per `testdata/sysfs-mi355x-spx/README.md`, the g0 to g7 files differ only by `gpu_id`. No test reads them. They are reference data. A code comment cites the static capture for the MI355X VRAM value. | reference only |
| `topology-parsing/` | KFD topology of one physical node with 1 GPU (`device_id` 26720, `simd_count` 256). Node 2 is a copy of node 1 to simulate two GPUs (see its `README.md`). | `amdgpu` property parsing and ROCr UUID tests |
| `topology-parsing-mi308/` | KFD topology of a node with 34 node directories, 32 of them with a render minor (`device_id` 29858, `gfx_target_version` 90402). GPU nodes carry `name`, `gpu_id`, `caches`, `io_links`, `p2p_links` and `mem_banks`. | `amdgpu` (`GetDevIdsFromTopology`), `allocator` pair-weight and best-effort tests, `exporter` partition-to-BDF test |
| `topo-mi300-cpx/` | KFD topology of an MI300 node in CPX mode (65 node directories, 63 with a render minor; `device_id` 29857, gfx942). | `allocator` best-effort tests, `TestComputeQueues` and `TestAddXGMIPeersOnCPXPartitions` (`plugin`) |
| `topo-mi210-xgmi-pcie/` | KFD topology of 8 MI210-class GPUs (`device_id` 29711, gfx90a) plus 2 CPU nodes. XGMI links (type 11) inside groups and PCIe links (type 2) otherwise. | `allocator` tests, `TestXGMIPeersOnTheMI210Fixture` and related |
| `debugfs-parsing/amdgpu_firmware_info` | One captured debugfs `amdgpu_firmware_info` file. | `TestParseDebugFSFirmwareInfo` |

The labeller tests build their own fake topology in temp directories (`fakeTopology` in `cmd/k8s-node-labeller/main_test.go`). They do not use these files.

### 9.4 What each CI job runs (`.github/workflows/`)

| Workflow / job | Trigger | What it runs |
|---|---|---|
| `ci.yml` / `test` ("Build and test") | PR, push to main, tags `v*`, manual | Checkout with submodules. Verify the submodule files exist and run `sh -n scripts/amd-vgpu-init.sh`. Build the Dockerfile `builder` stage. Run `go vet ./... && go test -race ./...` inside it. |
| `ci.yml` / `helm` | same | `helm lint ./helm/amd-gpu`. `helm template` with defaults and with `dp.cdi.enabled=true,dp.muslFailClosed.enabled=true,node_selector_enabled=true,monitor.enabled=true`. Helm v3.20.2. |
| `ci.yml` / `extra-images` | same | Build-only (no push) matrix: `labeller.Dockerfile`, `ubi-dp.Dockerfile`, `ubi-labeller.Dockerfile`, `linux/amd64`, GHA cache. |
| `ci.yml` / `image` | after `test` and `helm` | Build the main image with Buildx. Tags: `branch`, `pr`, `semver {{version}}`, `semver {{major}}.{{minor}}`, `sha`. Push to `ghcr.io/project-hami/amd-device-plugin` unless it is a PR. Attest build provenance on non-PR runs. |
| `codeql-analysis.yml` | push and PR to main, manual, cron `0 4 * * 6` (Saturdays) | The `image` job reads the ROCm image from the `rocm-runtime` line of `Dockerfile`. The `analyze` job runs CodeQL for Go inside that container after it installs the cgo build dependencies. Only for `Project-HAMi/amd-device-plugin`. |
| `scorecard.yaml` | branch protection rule, push to main, cron `30 2 * * 1` (Mondays) | OpenSSF Scorecard, SARIF upload. |
| `helm-chart-release.yml` | push to main touching `helm/**` or the workflow, manual | `helm/chart-releaser-action` publishes to the `helm-chart-release` branch. Release names are `<name>-helm-<version>`. `skip_existing: true`. |
| `dependabot.yml` | weekly | gomod, docker, github-actions, pip (`docs/sphinx`), gitsubmodule. |

- Action versions are pinned by commit SHA.
- There is no DCO workflow in `.github/workflows/`. But `CONTRIBUTING.md` lists DCO among the automated checks. It is enforced outside the repo's workflow files.
- amd-hami-core has its own `ci.yml`: unit tests with sanitizers, ThreadSanitizer, a build matrix for ROCm 6.2 and 7.2, and the glibc ABI check. It also has `codeql-analysis.yml` and `scorecard.yaml`.

---

## 10. Contributing

Files to read: `CONTRIBUTING.md`, `.github/PULL_REQUEST_TEMPLATE.md`, `.github/ISSUE_TEMPLATE/*`, `docs/contributing/development.md`.

- Code of Conduct: the HAMi community one (`CODE_OF_CONDUCT.md`, which adopts the CNCF code).
- Finding work:
  - Look at open issues and the `good first issue` label.
  - Comment on the issue before you start.
  - Issue templates: bug report, enhancement, question, good first issue.
  - Security reports are redirected to private advisories (`.github/ISSUE_TEMPLATE/config.yml`).
  - A bug report should include what you expected, what you saw, the GPU model, the ROCm version and reproduction steps.
- Workflow: fork, make a topic branch from `main`, send focused small PRs, add or update tests, and run `go build ./...`, `go vet ./...` and `go test ./...`.
- DCO: every commit must be signed off. Use `git commit -s -m "your message"`.
- PR template (`.github/PULL_REQUEST_TEMPLATE.md`). The sections, in order:
  - "What type of PR is this?" (add a `/kind` line).
  - "What this PR does / why we need it".
  - "Which issue(s) this PR fixes" (`Fixes #`).
  - "Special notes for your reviewer".
  - "Does this PR introduce a user-facing change?".
  - "AI Disclosure". The template says to disclose any AI assistance briefly. It says not to add AI co-author or assistance trailers to commits.
- `/kind` values listed in the template: `bug`, `cleanup`, `deprecation`, `design`, `documentation`, `failing-test`, `feature`, `flake`. `CONTRIBUTING.md` itself names `/kind bug`, `/kind feature` and `/kind documentation`.
- Commit style: `CONTRIBUTING.md` sets no format. The history uses conventional-commit subjects with the PR number. Examples: `feat(plugin): add -exclude_gpus and -hip_log_level options (#156)` and `fix(plugin): keep the device ID stable across operating modes (#159)`.
- Review and approval flow:
  - Maintainers review.
  - CI, DCO and CodeQL must pass.
  - When a reviewer is satisfied, they comment `/lgtm`.
  - An approver comments `/approve`.
  - The bot merges.
- OWNERS: this repo has no `OWNERS` file. The review flow comes from the HAMi project, which has its own root `OWNERS` file. Ask in the issue or PR who reviews a given area.
- Local build and test guidance for contributors: `docs/contributing/development.md`. It covers branch naming such as `feature/my-new-feature`, the `-s` sign-off, and running tests in the builder image.
- For amd-hami-core changes:
  - Read its `CONTRIBUTING.md`.
  - Changes to `libamvgpu_audit.c` or `hip_multiprocess_memory_limit.c` must be exercised on a real AMD GPU. The PR must say so.
  - It points to HAMi's contributing guide for the AI disclosure requirement.
- When a change touches both repos: this repo pins amd-hami-core as a submodule. So a library change reaches the plugin image only after the submodule pointer is bumped here. Dependabot proposes bumps. `CHANGELOG.md` records them.

---

## 11. Troubleshooting

Start with these commands (from `README.md` and `docs/user-guide/installation.md`):

```
kubectl -n kube-system get pods -l name=amd-gpu-dp-ds
kubectl -n kube-system logs ds/amd-gpu-device-plugin-daemonset
kubectl get node <node> -o jsonpath='{.status.allocatable.amd\.com/gpu}'
kubectl get node <node> -o jsonpath='{.metadata.annotations.hami\.io/node-amd-register}'
kubectl get events -n default --field-selector involvedObject.kind=Node
```

| Symptom | Likely cause and where to look |
|---|---|
| No `amd.com/gpu` on the node | The plugin registers resources only when `/sys/class/kfd` exists (`main.go`). Check `/dev/kfd`, `/dev/dri` and the `amdgpu` driver. If the log says `amdgpu driver unavailable. exiting with exit code 2`, then `/sys/module/amdgpu/drivers/` is missing (fatal in `GetAMDGPUs`). Make sure no other plugin (such as the upstream ROCm plugin) registers `amd.com/gpu`. The allocator warns `device id ... is not registered by this plugin; a second device plugin is likely running`. |
| GPU missing from the annotation | Look for the log lines `skip GPU <key>: AMD SMI did not return a UUID`, `libdrm capacity lookup failed` or `missing or invalid render node`. Or the GPU is in `-exclude_gpus`. Whole GPUs are never published without an AMD SMI UUID. An `AMDGPUMissing` Event is emitted when a GPU leaves the registration. |
| Annotation present but pods stay `Pending` | The pod must reach the HAMi scheduler (the webhook sets `schedulerName`). Read the scheduler's events. `Fit` reasons: `CardNotHealth`, `CardTimeSlicingExhausted` (all shares used), `CardInsufficientMemory`, `CardInsufficientCore`, `CardTypeMismatch`, `CardUUIDMismatch`, `NumaNotFit`, `ResourceQuotaNotFit`. HAMi reports `annos not found hami.io/node-amd-register` when the plugin never registered. |
| Pod rejected at creation with "container ... is privileged" | HAMi's webhook denies privileged containers that request HAMi-managed resources (`webhook.go`). Run unprivileged. See `example/pod/pytorch-non-privileged.yaml` (`hostIPC: true`, seccomp `Unconfined`, `privileged: false`). |
| Pod fails with `UnexpectedAdmissionError` | `Allocate` returned an error. Causes: no free CUs (often RDNA rounding to whole WGPs; `insufficient free CUs`), `CU allocation overlap ... between Pods`, an unresolvable device id (`no local GPU topology entry for device id`), or the node lock is owned by another pod (`node ... lock is owned by ...`). On these errors, the plugin sets `hami.io/bind-phase=failed` and releases the lock. |
| `no binding pod found on node` | Older plugin versions failed admission with this when the plugin found neither a node lock owner nor a Pending pod with `bind-time`, `bind-phase=allocating` and `hami.io/vgpu-node`. That means a pod not scheduled by HAMi, or an expired lock. Since amd-device-plugin#161, the plugin logs `no HAMi-tracked pod pending on node` and serves the kubelet's device ids as whole GPUs. Other lookup errors still fail the pod. |
| Slice ignores its memory limit or fails to start | The hook needs glibc 2.34 or newer. musl and static images never load it. Older glibc images fail with `GLIBC_2.34 not found`. dmem still caps VRAM where available. `dp.muslFailClosed.enabled` makes the plugin inspect the image. It leaves the hook out when dmem caps the slice. Otherwise it refuses with `... refusing to start unprotected`. This needs the containerd socket and data dir mounted with `HostToContainer` (RKE2 defaults in values; set the k3s paths on k3s). An image not yet on the node is pulled in the background. The first Pod that uses it is rejected with `image is not on the node yet`. Run it again once the pull has finished. |
| dmem cap not applied | At start, the plugin logs once `dmem: controller or systemd kubepods.slice not found under /sys/fs/cgroup` when the controller or the systemd cgroup driver is absent. The controller exists from kernel 6.14. On 6.8.0 the log line appeared and `/sys/fs/cgroup/cgroup.controllers` had no `dmem`. On 7.0.0, `dmem.capacity` listed `drm/<bdf>/vram`. The cap is written asynchronously (retries over about 20 s). Lowering `dmem.max` below current usage does not reclaim memory, so allocations made before the cap lands stay. Known gaps (header of `internal/pkg/dmem/dmem.go`): `hipMallocAsync` pools can spill into system memory, and whole-GPU pods on the same card get no cap. |
| `hipMemGetInfo` shows the whole card | Without `libamvgpu` (whole-GPU pod, musl, static), nothing virtualizes it. The dmem cap alone does not change `hipMemGetInfo`. |
| Multi-GPU pod sees GPUs in a different order than the masks | `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `HSA_CU_MASK` and `HIP_DEVICE_MEMORY_LIMIT_<i>` all use the container-local order. Index `i` is the same GPU in all four. A GPU without a ROCr UUID (KFD `unique_id` 0, APUs) is named by index in `ROCR_VISIBLE_DEVICES` (`rocrVisibleList`). |
| Slice not honoured by a child process | CU slices and memory limits are cooperative (`SECURITY.md`). A process that re-execs without `LD_AUDIT` runs outside the slice. This is not a bug. |
| Throughput collapses with many sharers on gfx12 | The default share count is 2 there for a reason (`README.md`, "Limits"). Do not raise `dp.splitCount` without measuring. |
| GPU shows unhealthy | Check Events (`AMDGPUUnhealthy`), the exporter socket `/var/lib/amd-metrics-exporter/`, and whether libdrm can open `/dev/dri/card<N>`. An unreachable exporter is logged once as `metrics exporter unreachable, using DRM health only until it returns`. |
| Mode change warnings | The `AMDGPUModeChangedInUse` Event means the node was not drained before `OPERATING_MODE` or the annotation changed. The mode is read only at plugin start. So restart the plugin pod after you change `hami.io/amd-operating-mode` or `hami.io/amd-compute-partition`. |
| Partition flip did not happen | The flip happens only at start, in partition mode with a target type, for idle GPUs. The log line is `compute-partition flip to <type> incomplete`. Busy GPUs and GPUs without profiles keep their mode. |
| Many `Count` slots but one GPU | This is expected. Kubelet sees `count` devices per GPU. So `amd.com/gpu` allocatable is GPUs times the share count. |
| Pod stuck allocating with a held node lock | The lock is per node (`hami.io/mutex.lock`). The HAMi scheduler treats it as expired after 5 minutes by default (`HAMI_NODELOCK_EXPIRE` on the scheduler overrides it). The HAMi scheduler will not hand an expired lock to another pod while its holder still awaits allocation (HAMi `nodelock.go`). |
| Monitor shows no container metrics | It needs cgroup v2 with the dmem controller and the systemd cgroup driver (`kubepods.slice`). The container must request an `amd.com/*` resource and appear in the node annotation by BDF. `hami_vgpumonitor_collect_success` 0 means part of a scrape failed. Read the monitor log. |
| Stale limits in a hand-run test | Delete the shared accounting file `/tmp/hipdevshr.cache` before each manual run of `libamvgpu` (amd-hami-core README, "Usage"). |
| Example `vllm-qpx-serve/device-config.yaml` | It shows `deviceSplitCount`, `defaultCores` and `defaultMemory` under `amd:`. HAMi's `AMDConfig` struct (`resourceCountName`, `resourceMemoryName`, `resourceMemoryPercentageName`, `resourceCoreName`, `runtimeClassName`) does not define them. Treat those keys as stale unless HAMi's config loader is shown to accept them. |

### 11.0 What is verified, and what needs bare metal

The real compute-partition flip (DPX, QPX, CPX) has not been verified on any hardware. It needs a bare metal machine with an Instinct GPU (MI300, MI325 or MI355).

A cloud virtual machine with an SR-IOV virtual function (VF) cannot do it. The VF reports `SPX` in `current_compute_partition` but has no `available_compute_partition` file, and AMD SMI refuses a change (status 2). The plugin treats such a GPU as fixed: it stays one soft, CU-sliced device with the normal split count in both operating modes, publishes no partition profiles, skips the `COMPUTE_PARTITION` flip with a log line, and keeps the plain `gpu` resource name under the `mixed` strategy.

Tested on a VF: both modes, the annotation overrides, switching the mode with Pods running, the naming strategies and all unit tests. Not tested anywhere: the flip itself, the registration of XCP partitions as hard devices on real hardware, and Pods placed on real partitions. To close it on a bare metal node, set `OPERATING_MODE=partition` and `COMPUTE_PARTITION=qpx` (or dpx, cpx), restart the plugin, and check the registration (hard entries, `count` 1, `mode`, per-partition VRAM and CU share), the kubelet ids (`<bdf>#<type>`) and a Pod on each partition.

### 11.1 Observed on a real MI325X

The test host had one GPU (a virtual function), k3s 1.37, and kernel 6.8.0 and then 7.0.0. Confirmed:

- Slices:
  - `HSA_CU_MASK` ranges are disjoint for concurrent Pods. Three 30 percent slices got `0:0-90`, `0:91-181` and `0:182-272`.
  - The rest wait for free cores.
  - The node lock is empty afterwards.
- The hook pins the mask. A process in a 25 percent slice that sets `HSA_CU_MASK` itself still runs at the slice speed (215 kernels/s). Without the hook it runs at 1015 kernels/s.
- Queue priority and allocator caps:
  - Priority 0 against 3 gives 144.7 and 72.3 kernels/s.
  - With a 1 GiB limit, `hipMalloc3D`, arrays, pools and `hipMemCreate` all return out of memory. `hipMemGetInfo` reports 1 GiB.
  - Three 1.5 GiB allocations under a 4 GiB limit give exactly two successes.
- dmem on kernel 7.0:
  - A 4096 MiB slice gets `dmem.max` of 4294967296 on its pod cgroup.
  - An 8 GiB allocation fails even without the hook.
  - The monitor reports per-container used and limit bytes.
- Operating modes:
  - The registered id is the same in `cu` and `partition` mode.
  - A mode change under a running Pod emits `AMDGPUModeChangedInUse`.
  - On the VF a compute-partition request is not attempted: the log says the GPU cannot change its compute partition (it has no `available_compute_partition` file) and it stays registered as a soft device with the normal split count. This holds in both operating modes, so switching the mode with Pods running did not break allocation (the id stays the same, masks stayed disjoint, and the scheduler counted the running Pod).
- HAMi side (merged pull requests re-run against HAMi master on the same host):
  - The AMD end-to-end suite passes (9 specs, 2 skipped).
  - The `gpumem-percentage`, ResourceQuota, `numa-bind`, `runtimeClassName`, mutex and comma-separated policy features all behave as their pull requests describe.
  - One bug: a GPU registered with an empty type is never picked, so the fallback of HAMi #2815 was unreachable. Fixed in HAMi #3212 (open, waits for review).
- Exporter health (tested with a stand-in that speaks the same gRPC protocol):
  - `unhealthy` drops allocatable to 0 and emits `AMDGPUUnhealthy`.
  - `healthy` restores it.
  - An outage is logged once.

Not checked:

- Real XGMI hardware and several real GPUs. The multi-GPU scheduler logic of HAMi #3187 and #3191 (link-bind, topology-aware, numa-bind) was run with the real scheduler on this machine, but with extra GPUs and XGMI links written into the node registration, so only the scheduling decision is covered.
- Open HAMi pull requests, all waiting for a review: #3212 (empty type fix), #3213 (AMD guide and examples), #3190, #3192, #3187, #3191.
- A real partition flip.
- RDNA behavior.
- A `hipMalloc` larger than VRAM (XNACK is off on the VF).
- A real exporter fault.

Other findings from that pass, fixed or open:

- A missing `OWNERS` file in both repositories.
- go1.26.8 with standard library vulnerabilities (fixed by moving to go1.26.9).
- A grpc development tag (moved to v1.84.0).
- The cordon annotation (`hami.io/device-cordon`) takes effect only after the scheduler's node cache sees it.

Debug logging:

- Raise `dp.logVerbosity` (`-v`). Level 4 adds topology parsing. Level 5 adds annotation decoding.
- For the hook, set `dp.hipLogLevel` (1 to 4) or `LIBHIP_LOG_LEVEL` in the pod.

---

## 12. Glossary

| Term | Meaning as used in this project |
|---|---|
| amd-smi / AMD SMI | AMD System Management Interface. The plugin links its C library (`libamd_smi`, SONAME `libamd_smi.so.26`). It uses it for UUIDs, product names, memory partitions, partition profiles and flipping the compute partition. The `amd-smi` CLI uses the same library. |
| APU / iGPU | Integrated GPU. It often has KFD `unique_id` 0, so it has no ROCr UUID. The plugin registers it and addresses it by index. |
| BDF | PCI Bus:Device.Function address, for example `<bdf>`. KFD topology spells it with a fourth colon. The plugin normalizes it (`normalizeBDF`, `dmem.NormalizeBDF`). It is used as the sysfs key, `custominfo.pciBDF`, the dmem region name and the exporter key. |
| besteffort / binpack / spread | `-allocator_policy` values. besteffort and binpack are the same policy (closest GPUs). spread picks the farthest. |
| CDI | Container Device Interface. With `-cdi_spec_dir`, the plugin writes `amd.com-gpu.json` (CDI version 0.6.0, kind `amd.com/gpu`, one device `card<N>` per GPU, `/dev/kfd` shared). It returns names like `amd.com/gpu=card0` instead of device nodes. It needs Kubernetes 1.28 or newer with the `DevicePluginCDIDevices` feature gate, and a CDI-enabled runtime (`docs/user-guide/configuration.md`). |
| CDNA | AMD's data-center GPU architecture line (Instinct; gfx9 family in this repo's fixtures). Single-CU masks. May support compute partitions. |
| CPX / DPX / QPX / SPX (TPX) | Compute partition modes: core partitioned (8 per MI355X), dual, quad, single. TPX is accepted by the code but absent from the MI355X profiles. |
| CU | Compute unit. `devcore` counts them. `amd.com/gpucores` is a percentage of them. |
| cu mode / partition mode | The plugin's operating modes (section 7.3). |
| cuPerWGP | A `custominfo` value: the CUs allocated together. It is 2 on gfx10 and later. |
| dmem | The kernel cgroup v2 controller for device memory. Files: `dmem.capacity` (regions such as `drm/<bdf>/vram`), `dmem.max`, `dmem.current`. The plugin writes caps. The monitor reads usage. |
| DRM | Direct Rendering Manager, the kernel graphics subsystem. Devices are `/dev/dri/card<N>` and `/dev/dri/renderD<M>`. |
| dp | Short for device plugin. It is the prefix of the `dp.*` Helm values. |
| ECC | Error-correcting memory. The exporter can report a GPU unhealthy after ECC errors (`installation.md`). |
| gfx / `gfx_target_version` | The GPU ISA family number, for example gfx942 or gfx1200 (section 7.1). |
| glog | The Google logging library used by the binaries. It is the source of `-v`, `-logtostderr` and the related flags. |
| HAMi | The heterogeneous-device sharing project whose scheduler this plugin serves. |
| HIP | AMD's CUDA-like programming runtime (`libamdhip64`, `hipMalloc`, `HIP_VISIBLE_DEVICES`). |
| HSA | Heterogeneous System Architecture, the runtime layer under HIP (`libhsa-runtime64`, `HSA_CU_MASK`, `hsa_queue_create`). |
| `HSA_CU_MASK` | The ROCm variable that selects usable CUs per GPU. The format is `GPU:CUlist[;GPU:CUlist]`, for example `0:0-15`. |
| Hard / soft entry | In the register annotation. A hard entry has `mode` set and `count` 1 (a whole partition). A soft entry has empty `mode` and is sliced by CU masks. |
| Heartbeat | The `-pulse` timer that makes `ListAndWatch` resend device health. |
| hwloc | Hardware locality library. It is linked only to log its version. |
| KFD | Kernel Fusion Driver. It provides `/dev/kfd` and the topology tree `/sys/class/kfd/kfd/topology/nodes/<n>/` (`properties`, `io_links`, `p2p_links`, `mem_banks`) that discovery reads. |
| libamvgpu | The `LD_AUDIT` library from amd-hami-core (section 2.4). |
| libdrm / libdrm_amdgpu | User-space DRM libraries. They are used for VRAM, CU count, family and firmware queries. |
| `LD_AUDIT` | The glibc dynamic-linker auditing interface. It lets a library intercept symbol binding. It needs glibc. musl does not support it. |
| NPS (NPS1, NPS2, NPS4, NPS8) | NUMA-per-socket memory partition modes of an Instinct GPU's memory (section 7.3). |
| NUMA | Non-uniform memory access node. It is published as `numa` and used in allocator weights and `amd.com/numa-bind`. |
| RDNA | AMD's graphics architecture line (gfx10 and later in this repo's logic). It uses WGP masks. |
| ROCm / ROCr | The AMD compute software stack and its runtime. ROCr is the HSA runtime part that reads `ROCR_VISIBLE_DEVICES`. |
| ROCr UUID | `GPU-<16 lowercase hex digits of the KFD unique_id>`. It is the spelling that `ROCR_VISIBLE_DEVICES` accepts (`GetROCrUUIDsFromTopology`). It is different from the AMD SMI UUID that is published as `id` for whole GPUs. |
| Slice | A pod's share of a GPU: a CU subset and/or a VRAM limit. |
| split count | The share count (`Count`): how many pods may share one GPU. |
| WGP | Workgroup processor. On RDNA it is a pair of CUs. It is the unit that CU masks must respect (section 7.2). |
| XCC | Accelerator complex die of an Instinct GPU. The MI355X has 8. Partition profiles are described as partitions x XCCs. |
| XCP | A compute partition device that the kernel exposes as `/sys/devices/platform/amdgpu_xcp_*`. It is registered as a hard entry in partition mode. |
| XGMI | AMD's GPU-to-GPU interconnect (Infinity Fabric link). KFD link type 11. |
| XNACK | The GPU page-fault retry capability. Managed-memory oversubscription on CDNA needs it (section 7.6). |
| `unique_id` | The KFD per-GPU identifier. It is 0 on APUs. |

End of guide.

---

# Part 2: amd-hami-core (the memory library)

The plugin loads this library into sliced pods. It lives in its own repository, [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core), and is described here so one guide covers both. Section numbers in this part start with C.

This guide is for someone who sees the repo for the first time. It covers `libamvgpu.so`, the `LD_AUDIT` library that amd-device-plugin uses to limit AMD GPU memory under HAMi.

## In one minute

1. `libamvgpu.so` is a small C library that the Linux dynamic linker loads into a GPU program through `LD_AUDIT`.
2. It watches HIP memory calls and refuses an allocation when the pod would go over its memory limit.
3. It also sets queue priority and keeps the pod's `HSA_CU_MASK` value from being changed by the program.
4. It does not mask compute units itself. The ROCm runtime does that.
5. A process that starts without `LD_AUDIT` runs outside the library, so this is cooperative sharing and not a hard security wall.

## C1. What it is and what it does not do

### What it is

- The repo is `Project-HAMi/amd-hami-core`. It holds one shared library, `libamvgpu.so`.
- It is licensed under Apache 2.0.
- amd-device-plugin loads it into pods that get a slice of a GPU.
- It is the AMD counterpart of the HAMi core library for NVIDIA. The accounting code follows the same pattern.
- It is plain C. It does not link against HIP or HSA. It declares the few types it needs by hand.

### What it does

- It enforces a per-pod, per-device GPU memory limit on HIP allocation calls.
- It makes `hipMemGetInfo`, `hipDeviceTotalMem` and the `totalGlobalMem` field of the device properties report the limit as the total memory.
- It counts memory across all processes of one container, through one shared file.
- It sets the priority class of every compute queue that the HIP runtime creates (`AMD_TASK_PRIORITY`).
- It pins three variables to the pod spec value: `HSA_CU_MASK`, `AMD_TASK_PRIORITY` and `HIP_OVERSUBSCRIBE`.
- It can serve `hipMalloc` from managed memory when `HIP_OVERSUBSCRIBE` is on, so a limit above the physical VRAM can be used.
- It restores some variables from the container's PID 1 when a child process lost them.

### What it does not do

- It does not mask compute units. The ROCm runtime enforces `HSA_CU_MASK`. The plugin sets it per pod.
- It is not a hard security boundary. `SECURITY.md` says a workload with enough privilege can bypass it. Examples are unsetting `LD_AUDIT`, linking the HIP runtime statically, or calling `ptrace` on itself.
- It does not count every kind of GPU memory. It counts the allocation calls listed in section 3.
- It does not intercept `hipMallocMipmappedArray`.
- It does not limit a process that runs without `LD_AUDIT`. A child that re-executes itself with a clean environment (for example `env -i`) is outside the library.
- It does not run for a whole-GPU pod. The plugin sets no `LD_AUDIT` for such a pod.

## C2. How it works

### LD_AUDIT in plain words

- `LD_AUDIT` names a library that the dynamic linker calls while it loads other libraries and binds symbols.
- The audit library exports `la_version`, `la_objopen` and `la_symbind64`.
- `la_version` runs first. The library logs a line and returns `LAV_CURRENT`.
- `la_objopen` runs for every loaded object. The library gives each object a cookie (a counter). When the file name contains `libamdhip64`, it remembers that cookie and the path. When the name contains `libhsa-runtime64`, it remembers the path.
- `la_objopen` returns `LA_FLG_BINDTO | LA_FLG_BINDFROM`, so `la_symbind64` is called for every symbol bound to and from each object.
- `la_symbind64` gets the symbol name. If the name is on the list, the library returns the address of its own wrapper in place of the real function.

### Why not LD_PRELOAD

- The source header says `LD_PRELOAD` breaks HIP start-up on ROCm 7.x because of internal `dlsym` and symbol resolution conflicts.
- `LD_AUDIT` works at the linker level. It can send only calls that come from outside the HIP library to the wrappers. Calls inside HIP stay untouched.

### Symbol binding

- The first time a symbol is seen, the library stores the real address (`sym->st_value`) in a function pointer such as `real_hipMalloc`.
- `INTERCEPT` returns the wrapper for outside callers. It returns the real function for calls that come from HIP itself (the caller's cookie equals the HIP cookie).
- `INTERCEPT_ALWAYS` returns the wrapper for every caller, HIP included. It is used for the read-only size queries, for `getenv` and for `hsa_queue_create`.
- Unknown symbols pass through unchanged.
- `la_symbind64` does not check whether a limit is set. If the library is loaded, it intercepts.

### Namespaces

- `LD_AUDIT` loads the library into a separate link-map namespace. That namespace has its own copy of glibc `environ`.
- A `setenv` in the library is therefore invisible to the HIP runtime, which lives in the default namespace.
- This is why the library intercepts `getenv` itself (`wrap_getenv`). The rewritten binding lives in the default namespace, so the runtime sees the value the library returns.
- To call the real HSA or HIP runtime from inside the audit namespace, the library reopens the library by path with `dlmopen(LM_ID_BASE, path, RTLD_NOW | RTLD_NOLOAD)`. `NOLOAD` returns the existing handle and never loads a second copy.

### No pthread calls in hot paths

- The binding code can be re-entered while the linker holds its lock. A lazy PLT lookup of `pthread_mutex_lock` would then deadlock.
- The library uses compiler atomics instead. The allocation tracker uses a spinlock built from `__sync_lock_test_and_set`. Initialization uses an atomic state flag (0 not started, 2 in progress, 1 ready).

### Lazy start-up

- The shared region is not created in `la_version`. The linker is still resolving symbols then.
- It is created on the first memory operation, in `ensure_shrreg_init`.
- The restore of environment variables also waits for that first call. Before `main` starts, glibc's `environ` is not the live one yet, and a `setenv` made too early is lost.

### The accounting flow for one allocation

1. The program calls `hipMalloc(&p, size)`. The wrapper runs.
2. The wrapper reads the thread's current device (set by the `hipSetDevice` wrapper, default 0).
3. `hip_reserve_device_memory` takes the semaphore, checks usage plus size against the limit, and adds the size to this process's slot in one step.
4. If it would go over the limit, the wrapper returns `hipErrorOutOfMemory` and never calls HIP.
5. Otherwise the wrapper calls the real function.
6. On failure, the reservation is given back.
7. On success, the pointer, size and device go into the allocation tracker.
8. `hipFree` removes the pointer from the tracker, calls the real free, and gives the bytes back to the device the memory came from. If the free fails, the entry goes back into the tracker.

The reserve-first step is what makes concurrent allocations safe. Without it, many threads could all pass the check and together exceed the limit.

### The allocation tracker (`alloc_tracker.h`)

- A hash table that maps pointer to size and device.
- Open addressing, linear probing, tombstones for deleted slots.
- Capacity is 16384 live allocations. When it is full, the allocation is freed again and reported as out of memory.
- A duplicate insert of the same pointer updates the entry.

### Pitched and shaped allocations

- `hipMallocPitch`, `hipMalloc3D` and `hipMemAllocPitch` reserve width times height first. After the call they know the real pitch. If the padded size is larger, they reserve the extra bytes. If the extra does not fit, the allocation is freed and the call returns out of memory.
- `hipMallocArray`, `hipMalloc3DArray` and the driver-API array creators compute the size from the channel format (`alloc_size.h`). If the size cannot be computed (empty, unknown format, overflow), the call passes through unaccounted.

### The shared accounting file

- Default path: `/tmp/hipdevshr.cache`. Override with `HIP_DEVICE_MEMORY_SHARED_CACHE`.
- The default path is inside the container's own `/tmp`. Processes of one container share one limit.
- The file is opened with `O_CREAT`, locked with `flock` during creation, sized with `ftruncate` and mapped with `mmap` (`MAP_SHARED`).
- The region has a magic number (`HMI2`), a process-shared semaphore, a limit per device, and 1024 process slots (`HIP_MAX_PROCS`).
- Each slot holds a pid, a status and, for each of 64 devices (`HIP_MAX_DEVICES`), the context, module and data bytes. The library only uses the data category today.
- Device indexes at or above 64 have no limit.
- Each process takes a slot on start-up and clears it at exit (`atexit`). A fork child registers a new slot through a `pthread_atfork` handler.
- Usage of a device is the sum over slots whose pid is still alive (checked through `/proc/<pid>/stat`).
- If all slots are used, dead ones are reclaimed.
- A slot that carries the current pid at start-up belongs to an older process (a restarted container that reused a persisted file). Its usage is reset.
- The semaphore wait has a 10 second timeout and up to 30 retries. If the lock owner is dead, exactly one waiter recovers the semaphore (compare and swap on `owner_pid`).
- A stuck lock or a process without a slot makes the reservation fail open. The allocation is allowed and not counted.
- Clear the file before a test run (`rm -f /tmp/hipdevshr.cache`). An old file keeps old limits.

### Reading variables early

- `safe_getenv` first calls `getenv`. If that returns nothing, it reads `/proc/self/environ`, then `/proc/1/environ` (the container's PID 1 has the full pod spec).
- If the first read finds no limit at all, the library retries on later calls, because some engine processes (vLLM or SGLang style) start HIP before their environment is ready.
- `read_proc_file` reads the whole file in a growing buffer. A fixed buffer once cut `4096m` into `409`.

### Size parsing

- `parse_memory_size` accepts a whole number with an optional `K`, `M`, `G` or `T`, in either case, and one trailing newline.
- Anything else means no limit: `1.5G`, `-1`, `4096 M`, `4096MB`, text, an empty string, a missing value and overflow.
- A bad value is logged and never becomes a tiny or huge limit.

### Environment pinning

- `env_is_pinned` is true for `HSA_CU_MASK`, `AMD_TASK_PRIORITY` and `HIP_OVERSUBSCRIBE`.
- For these, `wrap_getenv` returns the pod spec value from `/proc/1/environ` when it exists. It does so even when the process set its own copy or removed it.
- Without a pod spec value, the process's own value is used.
- Other tracked variables (`ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `ROC_GLOBAL_CU_MASK`) only fall back to the pod spec when the process lost them.
- Why: the ROCm runtime reads `HSA_CU_MASK` once at start and already clamps later mask calls to it. The only way out of the slice is to change the variable before the runtime starts. Pinning closes that way.

### Queue priority

- The HIP runtime creates every stream's queue through `hsa_queue_create`. The library wraps that call, including calls made by HIP itself.
- After a successful create, it reads `AMD_TASK_PRIORITY` through `wrap_getenv` and calls `hsa_amd_queue_set_priority`.
- `queue_priority_level`: `0` gives the high class. Any higher whole number gives the low class. Empty, negative, text and overflow values ask for nothing, and the queue keeps its default.
- The function `hsa_amd_queue_set_priority` is found with `dlmopen` and `dlsym` on the HSA runtime path, on first use.

### Oversubscription

- With `HIP_OVERSUBSCRIBE` set to `true` (any case) or `1`, `wrap_hipMalloc` calls `hipMallocManaged` instead, with the global attach flag.
- The memory limit still applies. The reservation is made before.
- If the program never called `hipMallocManaged`, the function is not bound yet. The wrapper looks it up in the runtime once.
- If it cannot be found, a warning is logged and the normal `hipMalloc` is used.

## C3. Every intercepted call

The table follows `la_symbind64` in `src/hip/libamvgpu_audit.c`. "Outside" means only callers outside the HIP library get the wrapper. "Always" means every caller does.

| Symbol | Mode | What the wrapper does |
|---|---|---|
| `hipMalloc` | outside | Reserve, allocate (from managed memory if oversubscribed), track. |
| `hipFree` | outside | Remove from tracker, free, release bytes. |
| `hipMemGetInfo` | always | Real call, then total becomes the limit and free becomes limit minus usage, never above the real free. |
| `hipSetDevice` | outside | Real call, then remember the device for this thread. |
| `hipMallocManaged` | outside | Reserve, allocate, track. |
| `hipMallocAsync` | outside | Reserve, allocate, track. |
| `hipFreeAsync` | outside | Remove from tracker, free, release bytes. |
| `hipMallocPitch` | outside | Reserve width times height, then the padding. |
| `hipExtMallocWithFlags` | outside | Reserve, allocate, track. |
| `hipMalloc3D` | outside | Reserve extent size, then the padding. |
| `hipMallocArray` | outside | Reserve size from channel format. |
| `hipMalloc3DArray` | outside | Reserve size from channel format. |
| `hipFreeArray` | outside | Remove from tracker, free, release bytes. |
| `hipArrayCreate` | outside | Driver-API array, size from format and channels. |
| `hipArray3DCreate` | outside | Driver-API 3D array. |
| `hipArrayDestroy` | outside | Remove from tracker, destroy, release bytes. |
| `hipMemAllocPitch` | outside | Driver-API pitched allocation, with padding. |
| `hipMallocFromPoolAsync` | outside | Reserve, allocate from the pool, track. |
| `hipMemCreate` | outside | Reserve the physical memory of a virtual memory handle on the current device. |
| `hipMemRelease` | outside | Remove from tracker, release the handle, release bytes. |
| `hipDeviceTotalMem` | always | Real call, then clamp to the limit. |
| `hipGetDevicePropertiesR0600` | always | Clamp `totalGlobalMem` (offset 288). |
| `hipGetDeviceProperties` | always | Clamp `totalGlobalMem` (offset 256, older layout). |
| `hipGetDevicePropertiesR0000` | always | Clamp `totalGlobalMem` (offset 256). |
| `getenv` | always | Serve pinned and fallback variables from the pod spec. |
| `hsa_queue_create` | always | Create the queue, then set its priority. |

Notes on the table:

- The clamp only lowers the total. A limit above the real total changes nothing (`clamp_total`).
- A failed call returns `hipErrorNotInitialized` when the real function pointer is not known yet.
- A pointer that is not in the tracker (for example allocated before the library saw it) is freed without accounting.

## C4. Environment variables

| Variable | Set by | Effect |
|---|---|---|
| `LD_AUDIT` | the plugin (`/usr/local/vgpu/libamvgpu.so`), slices only | Loads the library. |
| `HIP_DEVICE_MEMORY_LIMIT_<i>` | the plugin, one per container-local device index, value like `4096m` | Memory limit of device `i` (0 to 63). Invalid means no limit. |
| `HIP_DEVICE_MEMORY_LIMIT` | optional | Limit for every device without its own variable. |
| `HIP_DEVICE_MEMORY_SHARED_CACHE` | optional | Path of the shared accounting file. Default `/tmp/hipdevshr.cache`. |
| `ACTIVE_OOM_KILLER` | optional | Value `true` clears the slots of dead processes when an allocation would go over the limit, then checks again. |
| `LIBHIP_LOG_LEVEL` | the plugin, when its `-hip_log_level` is above 0 | 1 error, 2 warn (default), 3 info, 4 debug. Values outside 1 to 4 give the default. Messages go to stderr. |
| `HSA_CU_MASK` | the plugin | Read by ROCm to limit compute units. The library serves the pod spec value. |
| `AMD_TASK_PRIORITY` | the user, in the pod spec | `0` high class, higher is low class, unset keeps the default. Pinned. |
| `HIP_OVERSUBSCRIBE` | the plugin, when its memory scale is above 1 | `true` or `1` serves `hipMalloc` from managed memory. Pinned. |
| `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `ROC_GLOBAL_CU_MASK` | the plugin or the user | Read by ROCm. The library only restores them when the process lost them. |
| `CUDA_DEVICE_MEMORY_LIMIT`, `CUDA_DEVICE_MEMORY_LIMIT_0` | legacy | In the restore list. The library does not read them for limits. |

The restore list for the library's own use (`internal_restore_vars`) is: `HIP_DEVICE_MEMORY_LIMIT`, `HIP_DEVICE_MEMORY_LIMIT_0`, `HIP_DEVICE_MEMORY_LIMIT_1`, `HIP_DEVICE_MEMORY_SHARED_CACHE`, `CUDA_DEVICE_MEMORY_LIMIT`, `CUDA_DEVICE_MEMORY_LIMIT_0`, `LIBHIP_LOG_LEVEL` and `ACTIVE_OOM_KILLER`. They are filled from `/proc/1/environ` when missing.

## C5. How amd-device-plugin uses it

- The plugin ships the library in the image and mounts it at `/usr/local/vgpu/libamvgpu.so` for pods that get a slice.
- For such a pod it sets `LD_AUDIT`, one `HIP_DEVICE_MEMORY_LIMIT_<i>` per device (in MiB, like `4096m`), and `HSA_CU_MASK` for the compute slice.
- It sets `LIBHIP_LOG_LEVEL` when asked, and `HIP_OVERSUBSCRIBE=true` when the device memory scale is above 1.
- The plugin does not set `AMD_TASK_PRIORITY`. The user sets it in the pod spec.
- A whole-GPU pod gets no `LD_AUDIT`. The library never runs in it.
- Where the kernel has the dmem cgroup controller, the plugin also sets a cgroup cap. That cap covers what the library does not intercept, such as `hipMallocMipmappedArray`.
- The plugin documents that workload images need glibc 2.34 or newer. That comes from this library's build baseline (section 8).

## C6. Limits and known gaps

- Calls outside the library: a process without `LD_AUDIT` (for example after `env -i`), a statically linked HIP runtime, and musl-based binaries run with no limit. Only a limit in the kernel driver can close this.
- `hipMallocMipmappedArray` is not intercepted. The dmem cgroup cap helps where it exists.
- Only the data category is counted. Contexts and code objects that HIP allocates on its own are not.
- At most 64 devices and 1024 processes per container. At most 16384 live tracked allocations per process.
- A device index of 64 or more has no limit.
- The device of an allocation is the thread's current device. A thread that never calls `hipSetDevice` uses device 0.
- A virtual memory handle that is shared or retained elsewhere leaves the accounting at its first `hipMemRelease` (a `ponytail:` comment in the source marks this).
- Fail open: if the lock cannot be taken or the process has no slot, the allocation goes through uncounted.
- The check uses a size estimate for shaped arrays. If the format is unknown or the size overflows, the call is not accounted.
- A limit larger than the real VRAM does not make memory appear. Only `HIP_OVERSUBSCRIBE` lets `hipMalloc` use managed memory.
- A limit is read once, when the region is created. A kept old file keeps old limits.
- The pins read `/proc/1/environ`. Outside a container, PID 1 is the host init. Test with care.
- Verified by the README on: MI300X, RX 9060 XT (gfx1200), RX 9070 XT (gfx1201), and ROCm 6.2, 7.0, 7.1 and 7.2.

## C7. What was observed on a real MI325X

These were measured by hand on one MI325X, on a virtual function (so partition mode flips were not possible).

- Compute slice: a compute kernel ran at 215 kernels/s in a 25 percent slice (mask `0:0-75`).
- Setting `HSA_CU_MASK` with `setenv` before HIP starts did not widen the slice with the library (still 215 kernels/s). Without the library it did (1015 kernels/s). This is the pin working.
- Priority: two processes on the same 25 percent slice got 144.7 and 72.3 kernels/s with `AMD_TASK_PRIORITY` 0 and 3. Without it they got 108.7 and 108.5.
- Memory calls: with a 1 GiB limit, `hipMalloc3D`, `hipMallocArray`, `hipMalloc3DArray`, `hipMallocFromPoolAsync` and `hipMemCreate` all returned out of memory. `hipMemGetInfo` reported 1 GiB.
- Concurrency: three concurrent 1.5 GiB allocations under a 4 GiB limit gave exactly two successes, in each of three rounds.
- Not checked: a `hipMalloc` larger than the VRAM, served from managed memory. XNACK was off on the virtual function.

## C8. Repo layout

```
.github/
  ISSUE_TEMPLATE/    bug-report.md, enhancement.md, good-first.md, question.md, config.yml
  PULL_REQUEST_TEMPLATE.md
  dependabot.yml
  rocm/              pinned ROCm build images, one Dockerfile per version
  workflows/         ci.yml, codeql-analysis.yml, scorecard.yaml
src/
  hip/               the audit library and its pure helpers
  include/           small shared headers
  multiprocess/      the shared accounting region
test/                unit tests and the glibc check
```

Every file:

| Path | What it is |
|---|---|
| `README.md` | What it does, build, usage, variables, limits, tests. |
| `CONTRIBUTING.md` | Build and test commands, how to submit a change. |
| `SECURITY.md` | Private reporting through the HAMi advisory form, and what is in scope. |
| `CODE_OF_CONDUCT.md` | Community rules. |
| `LICENSE` | Apache 2.0. |
| `security-insights.yml` | Project security metadata. |
| `Makefile.hip` | Entry point for the build. Links the CMake file into `build-hip/` and runs cmake and make. Targets: `all`, `test`, `clean`, `install`. |
| `CMakeLists.txt.hip` | The CMake build. It is not named `CMakeLists.txt` on purpose. |
| `Dockerfile.hip` | Builds in a ROCm image, runs the checks, exports the files. |
| `.gitignore` | Ignore rules. |
| `.github/PULL_REQUEST_TEMPLATE.md` | PR template: kind, what and why, issue, notes, user-facing change, AI disclosure. |
| `.github/dependabot.yml` | Keeps actions and the pinned image digests current. |
| `.github/rocm/rocm-6.2.Dockerfile` | One `FROM` line: pinned ROCm 6.2 image on Ubuntu 22.04. CI reads it. |
| `.github/rocm/rocm-7.2.Dockerfile` | One `FROM` line: pinned ROCm 7.2 image on Ubuntu 24.04. |
| `.github/workflows/ci.yml` | Unit tests, build matrix, glibc check. |
| `.github/workflows/codeql-analysis.yml` | CodeQL scan of the C code. |
| `.github/workflows/scorecard.yaml` | OpenSSF Scorecard run. |
| `src/hip/libamvgpu_audit.c` | The audit library: LD_AUDIT hooks, wrappers, env pinning, queue priority. |
| `src/hip/alloc_tracker.h` | Pointer to size hash table. |
| `src/hip/alloc_size.h` | Size of shaped allocations, and the total clamp. |
| `src/hip/env_policy.h` | Which variables are pinned, and which value `getenv` returns. |
| `src/hip/oversubscribe.h` | Parses `HIP_OVERSUBSCRIBE`. |
| `src/hip/queue_priority.h` | Maps `AMD_TASK_PRIORITY` to an HSA priority. |
| `src/hip/test_memory_limit.c` | The on-GPU test program. |
| `src/include/libamvgpu.h` | Variable names and the default cache path. |
| `src/include/glibc_compat.h` | Keeps the build at the glibc 2.34 baseline. |
| `src/include/hip_log_utils.h` | Log macros and `LIBHIP_LOG_LEVEL` handling. |
| `src/include/memory_size.h` | Parses sizes like `4096m`. |
| `src/include/proc_file.h` | Reads a whole `/proc` file. |
| `src/multiprocess/hip_multiprocess_memory_limit.h` | Region layout, constants and function list. |
| `src/multiprocess/hip_multiprocess_memory_limit.c` | Region init, slots, lock, reserve and release, cleanup. |
| `test/check_glibc_abi.sh` | Fails if the library needs a glibc symbol newer than 2.34. |
| `test/test_alloc_size.c` | See section 10. |
| `test/test_alloc_tracker.c` | See section 10. |
| `test/test_env_policy.c` | See section 10. |
| `test/test_init.c` | See section 10. |
| `test/test_memory_size.c` | See section 10. |
| `test/test_oversubscribe.c` | See section 10. |
| `test/test_queue_priority.c` | See section 10. |
| `test/test_reserve.c` | See section 10. |
| `test/test_retry.c` | See section 10. |
| `test/test_shrreg.c` | See section 10. |

A good reading order: `README.md`, then `libamvgpu_audit.c` from the bottom (`la_symbind64`) upward, then `hip_multiprocess_memory_limit.c`.

## C9. How to build

### Local build

```
make -f Makefile.hip                     # build-hip/libamvgpu.so
make -f Makefile.hip test                # also build-hip/test_memory_limit (needs hipcc)
make -f Makefile.hip ROCM_HOME=/opt/rocm-6.4.0
make -f Makefile.hip clean
make -f Makefile.hip install             # /usr/local/lib, then ldconfig
```

- You need ROCm headers, `cmake` and a C compiler. `ROCM_HOME` defaults to `/opt/rocm`.
- The makefile creates `build-hip/`, links `CMakeLists.txt.hip` in as `CMakeLists.txt` and links `src` next to it. CMake has no flag for another file name, so this is the trick.
- Flags: `-fPIC -D_GNU_SOURCE -std=gnu11 -Wall -Wextra`. Release adds `-O2 -fvisibility=hidden` and strips local symbols. Debug adds `-g -O0 -DDEBUG`.
- Only `la_version`, `la_objopen` and `la_symbind64` are exported (marked with default visibility).
- The build embeds the short git hash when git is available.
- The library links `dl` and `pthread`.

### Docker build

```
docker build -f Dockerfile.hip \
  --build-arg ROCM_IMAGE=rocm/dev-ubuntu-24.04:7.2 \
  -t libamvgpu-builder .
docker run --rm -v $(pwd)/dist:/dist libamvgpu-builder
# dist/libamvgpu.so and dist/test_memory_limit
```

- Match `ROCM_IMAGE` to the host ROCm. ROCm 6.x uses the Ubuntu 22.04 image. ROCm 7.x uses the Ubuntu 24.04 image.
- The default image is the Ubuntu 22.04 one with ROCm 6.2, pinned by digest. Build on the older Ubuntu to keep the glibc level low.
- The builder stage runs `make clean all`, then `make test`, then `check_glibc_abi.sh`, then checks that `nm -D` shows `la_version` and `la_symbind64`.
- The `output` stage holds only the two files. The default stage copies them to `/dist`.
- For development, mount the source in the builder image and run `make -f Makefile.hip clean all` inside.

### The glibc 2.34 baseline

- Workload images may carry an older glibc (Ubuntu 22.04 ROCm images use 2.35). The library must run there.
- With a new glibc, `_GNU_SOURCE` turns on C23 mode, and `strtol` and `strtoull` are renamed to `__isoc23_*`. That adds a hard `GLIBC_2.38` dependency.
- `src/include/glibc_compat.h` turns that off. It undefines both macro spellings (`__GLIBC_USE_C2X_STRTOL` and `__GLIBC_USE_C23_STRTOL`). It must be included before `<stdlib.h>`.
- `test/check_glibc_abi.sh <lib>` lists the `GLIBC_*` versions in `objdump -T` and fails when one is newer than 2.34. It checks every version, not one fixed release.

## C10. Tests

All unit tests need no GPU. They use `assert`, print a `PASS` line, and exit non-zero on failure. Run them without `-DNDEBUG`.

### Pure helper tests

Build: `gcc -Wall -Wextra -Werror -o /tmp/<name> test/<name>.c -I src/hip`

| Test | What it checks |
|---|---|
| `test_alloc_tracker` | Init, insert and lookup, remove (size and device returned), NULL pointers rejected, duplicate insert updates, free then reuse of an address, 1000 entries, device tracking, 200 colliding pointers with removal and tombstone chains, tombstone reuse, full table (insert fails at 16384, works after one removal), and a 500 allocation interleaved pattern like PyTorch. |
| `test_env_policy` | The pod `HSA_CU_MASK` wins over a changed or removed process value. A missing pod value leaves the process alone. Other tracked variables only fall back when lost. Untracked names return NULL. `AMD_TASK_PRIORITY` and `HIP_OVERSUBSCRIBE` are pinned too. |
| `test_memory_size` | `4096m`, `16G`, `512k`, `1T`, plain bytes and a trailing newline parse. `1.5G`, `-1`, `4096 M`, `4096MB`, text, empty, NULL and overflow return 0. |
| `test_queue_priority` | `0` is high. `1` and `7` are low. NULL, empty, `-1`, `high`, `1x` and a huge number ask for nothing. |
| `test_alloc_size` | Array size from channel bits (float4, 1D and 2D with zero height or depth, odd bit layouts round up), zero results for empty or negative channels, overflow gives 0, driver-API formats and channels, unknown format gives 0, extent size and overflow, and the total clamp (limit lower, no limit, limit above the GPU). |
| `test_oversubscribe` | `true`, `TRUE` and `1` are on. `false`, `0`, empty, `yes` and NULL are off. |

### Shared region tests

Build: `gcc -O2 -Wall -Wextra -Werror -pthread -o /tmp/<name> test/<name>.c src/multiprocess/hip_multiprocess_memory_limit.c`

| Test | What it checks |
|---|---|
| `test_reserve` | Eight threads each reserve 300 MiB under a 1 GiB limit. Exactly three succeed. Usage equals three chunks. After release, usage is zero. A device with no limit returns 1 (nothing reserved). |
| `test_init` | Sixteen threads call `hip_shrreg_init` together. Every caller sees the limit already loaded and the process takes exactly one slot. |
| `test_retry` | With no limit set, eight threads call init 200 times each. This must not race on the shared `/proc` buffer. The limit stays 0. |
| `test_shrreg` | A stale slot with the current pid and 512 MiB is reset. A dead lock owner is recovered once, not twice. The semaphore ends at 1. This test takes about one lock timeout (10 seconds). |

### Run them all

```
for t in test_alloc_tracker test_env_policy test_memory_size test_queue_priority test_alloc_size test_oversubscribe; do
  gcc -Wall -Wextra -Werror -o /tmp/$t test/$t.c -I src/hip && /tmp/$t
done
for t in test_reserve test_shrreg test_init test_retry; do
  gcc -O2 -pthread -o /tmp/$t test/$t.c src/multiprocess/hip_multiprocess_memory_limit.c && /tmp/$t
done
```

Note: the README and CONTRIBUTING list fewer tests than CI runs. They leave out `test_queue_priority`, `test_alloc_size` and `test_oversubscribe`. Run the full list above.

### Sanitizers

CI adds this to every test:

```
san="-Wall -Wextra -Werror -fsanitize=address,undefined -fno-sanitize-recover=all"
gcc $san -o /tmp/test_x test/test_x.c -I src/hip
gcc -O2 $san -pthread -o /tmp/test_reserve test/test_reserve.c src/multiprocess/hip_multiprocess_memory_limit.c
```

### ThreadSanitizer

The threaded tests run again under ThreadSanitizer. A data race in the shared region would lose or double count memory. TSan exits non-zero on any report.

```
for t in test_reserve test_init test_retry; do
  gcc -O1 -g -fsanitize=thread -pthread -o /tmp/${t}_tsan test/$t.c src/multiprocess/hip_multiprocess_memory_limit.c
  /tmp/${t}_tsan
done
```

ASLR note:

- TSan needs a fixed memory layout. On some kernels, high address randomization makes it stop with an error like "unexpected memory mapping".
- Fix it for one run: `setarch $(uname -m) -R /tmp/test_reserve_tsan`.
- Or lower the randomization for the machine: `sudo sysctl vm.mmap_rnd_bits=28`.
- Do not mix ASan and TSan in one binary.

### The glibc check

```
test/check_glibc_abi.sh dist/libamvgpu.so
```

### The on-GPU test (needs an AMD GPU and ROCm)

```
rm -f /tmp/hipdevshr.cache
LD_AUDIT=dist/libamvgpu.so HIP_DEVICE_MEMORY_LIMIT_0=1G LIBHIP_LOG_LEVEL=3 dist/test_memory_limit
```

`src/hip/test_memory_limit.c` does this:

1. Prints the device count and name, then calls `hipSetDevice(0)`.
2. Reads `hipMemGetInfo`. With a limit, the total is the limit.
3. Allocates 256 MB twice. Both must work.
4. Tries 2 GB. It expects out of memory. If it succeeds while the limit is below 2.5 GB, the test prints FAIL and returns 1.
5. Frees both, prints the final memory, and prints "All tests passed."

Other runs: PyTorch (`torch.cuda.mem_get_info()` with `HIP_DEVICE_MEMORY_LIMIT_0=4G`) and a CU slice (`HSA_CU_MASK=0:0-15`).

Remember to clear the cache file before each run.

## C11. CI jobs

| Workflow and job | What it does |
|---|---|
| `ci.yml` / `unit-tests` | Builds every unit test with `-Wall -Wextra -Werror` and ASan plus UBSan, runs them, then runs the threaded tests under TSan. |
| `ci.yml` / `build` | Matrix of ROCm 6.2 and 7.2. Reads the pinned image from `.github/rocm/`, builds `Dockerfile.hip` with it. A green build means the glibc check and the `LD_AUDIT` symbol check passed. |
| `ci.yml` / `glibc-abi-check` | Needs `build`. Builds the `output` stage with ROCm 6.2, exports `dist/`, runs `check_glibc_abi.sh` on it. |
| `codeql-analysis.yml` / `analyze` | CodeQL for C and C++, inside the pinned ROCm 6.2 image, with a manual build (`make -f Makefile.hip clean all`). Runs on push, pull request and weekly. |
| `scorecard.yaml` / `analysis` | OpenSSF Scorecard, weekly and on push. Results go to the security tab. |

Facts about the workflows:

- CI runs on pull requests, on pushes to `main` and by hand.
- Actions are pinned by commit hash. Image digests are pinned too. Dependabot updates both.
- CI does not run the on-GPU test. That is on you (section 12).

## C12. How to contribute

- The repo follows the HAMi project's Code of Conduct and Contributing guide. Read them first.
- Fork, branch, keep the change small. Unrelated cleanup goes in its own PR.
- Add a test for every change. It should fail without your fix.
- Run the unit tests (with sanitizers), the glibc check and, for C changes, a build for ROCm 6.2 and 7.2 if you can.
- If you touch `libamvgpu_audit.c` or `hip_multiprocess_memory_limit.c`, run the on-GPU test on real AMD hardware. Say so in the PR. A change that was never run on a real GPU cannot be reviewed.
- DCO: sign every commit with `git commit -s`. To fix one that is not signed: `git commit --amend --no-edit -s`, then push with `--force-with-lease`.
- Do not add AI co-author or assistance trailers to commits.
- PR template (`.github/PULL_REQUEST_TEMPLATE.md`) sections:
  - what type of PR (add a `/kind` line: bug, cleanup, deprecation, design, documentation, failing-test, feature or flake),
  - what this PR does and why,
  - which issues it fixes (`Fixes #`),
  - notes for the reviewer,
  - whether it has a user-facing change,
  - AI disclosure: say briefly if AI helped. Where the guide asks for it, one short line is enough.
- Issue templates and their labels: Bug Report (`bug`), Enhancement Request (`enhancement`), Good First Issue (`good first issue`), Question (`question`). Give the ROCm version and the GPU model in a bug.
- Security issues: do not file a public issue. Use the HAMi advisory form named in `SECURITY.md`. A workload that only exceeds its own quota is not a new vulnerability. A way to reach another tenant's GPU memory is in scope.
- A maintainer reviews and may ask for changes before merge.

## C13. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| No limit at all in a test | The old cache file kept the old (empty) limits. Run `rm -f /tmp/hipdevshr.cache`. Also check `LD_AUDIT` is a valid path and the variable name has the device index (`HIP_DEVICE_MEMORY_LIMIT_0`). |
| The limit seems ignored | The value is invalid (for example `1.5G` or `4096MB`). Look for "Ignoring invalid" in the log. Use a whole number with `K`, `M`, `G` or `T`. |
| Need to see what happens | Set `LIBHIP_LOG_LEVEL=3` (info) or `4` (debug). Lines start with `[HAMI-core-hip`. |
| Allocation fails with out of memory below the limit | Another process of the container holds memory in the shared file. Or a dead process left a slot. Try `ACTIVE_OOM_KILLER=true`. |
| Lock timeout warnings | A process died while holding the lock. The library recovers it after one timeout (10 seconds). |
| "No available process slots" | More than 1024 processes use the file. Clear stale files. |
| "allocation tracker full" | More than 16384 live allocations in one process. |
| App fails to start on an old image | The library needs glibc 2.34 or newer in the image. |
| A program escapes the limit | It runs without `LD_AUDIT` (clean `env`, static HIP, musl). This is the known gap. |
| `HSA_CU_MASK` changes have no effect | That is the pin. The pod spec value is served. |
| Library built but `check_glibc_abi.sh` fails | A call pulled in a newer glibc symbol. Check that `glibc_compat.h` is included first and avoid new libc calls. |
| TSan stops with a memory mapping error | ASLR. Use `setarch $(uname -m) -R` or lower `vm.mmap_rnd_bits` (section 10). |
| Build cannot find ROCm | Set `ROCM_HOME`. The build stops when `/opt/rocm` is missing and `ROCM_HOME` is unset. |
| `hipMalloc` hangs or crashes at start-up with the library | Do not use `LD_PRELOAD` for this library. Use `LD_AUDIT`. Also avoid new `pthread` calls in code that runs during binding. |

## C14. Glossary

- **AMD_TASK_PRIORITY**: variable that picks the priority class of a pod's compute queues.
- **ASLR**: address space layout randomization. It places memory at random addresses.
- **audit library**: a library named in `LD_AUDIT` that the linker calls when it loads objects and binds symbols.
- **CU**: compute unit, the compute building block of an AMD GPU.
- **CU mask**: a list of the CUs a process may use. `HSA_CU_MASK=0:0-75` means GPU 0, CUs 0 to 75.
- **cookie**: a number the audit library attaches to each loaded object, used to tell who is calling.
- **dmem cgroup**: a kernel controller that caps device memory per cgroup.
- **DCO**: Developer Certificate of Origin. You agree to it with `git commit -s`.
- **fail open**: let the call through when the check cannot run.
- **gfx1200, gfx1201**: GPU architecture names (RX 9060 XT, RX 9070 XT).
- **glibc baseline**: the oldest glibc the library must run on (2.34).
- **HAMi**: the Kubernetes project for GPU sharing.
- **HIP**: AMD's GPU programming interface, like CUDA.
- **HSA**: the low-level runtime under HIP. It creates the queues.
- **hipMallocManaged**: allocation that the driver can move between VRAM and host RAM.
- **la_symbind64**: the audit function that picks the address for each bound symbol.
- **link-map namespace**: a separate set of loaded libraries inside one process. `LD_AUDIT` libraries live in their own.
- **LD_AUDIT**: environment variable that names an audit library.
- **LD_PRELOAD**: environment variable that loads a library first. Not used here.
- **oversubscription**: giving a pod a memory limit above the real VRAM.
- **PLT**: procedure linkage table, where calls to shared library functions are resolved.
- **ROCm**: AMD's GPU software stack.
- **shared region**: the mmap'd accounting file (`/tmp/hipdevshr.cache`).
- **slice**: the part of a GPU (compute units and memory) given to one pod.
- **slot**: one process's entry in the shared region.
- **TSan, ASan, UBSan**: ThreadSanitizer, AddressSanitizer and UndefinedBehaviorSanitizer.
- **VF**: virtual function, a GPU share exposed to a virtual machine.
- **VRAM**: the memory on the GPU.
- **XNACK**: a GPU feature that lets the GPU handle page faults. Needed to use managed memory beyond VRAM.
- **XGMI**: AMD's link between GPUs. This library does not use it.
