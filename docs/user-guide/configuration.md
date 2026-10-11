# Configuration

## Helm values and plugin flags

The chart passes its values to the plugin as flags. The [chart README](https://github.com/Project-HAMi/amd-device-plugin/blob/main/helm/amd-gpu/README.md) lists every value.

| Helm value | Flag | Default | Description |
|---|---|---|---|
| `dp.splitCount` | `-split_count` | `0` | How many pods may share one GPU. `0` picks it per GPU: 2 on gfx12, otherwise 10. Any other value applies to every GPU. |
| `dp.allocatorPolicy` | `-allocator_policy` | `besteffort` | How kubelet picks GPUs for multi-GPU pods: `besteffort` (same as `binpack`, closest GPUs) or `spread` (farthest GPUs). See [Resource allocation](resource-allocation.md). |
| `dp.logVerbosity` | `-v` | `2` | Log verbosity; 4 adds topology parsing, 5 adds annotation decoding. |
| `dp.healthPulse` | `-pulse` | `10` (chart), `0` (flag) | Seconds between GPU health checks. `0` disables them. |
| `dp.dmemBackend` | `-dmem_backend` | `true` | Also cap a slice's VRAM with the kernel dmem cgroup controller. |
| `dp.reportNodeCapacity` | `-report_node_capacity` | `false` | Publish the healthy GPUs' memory (`amd.com/gpumem`, MiB) and compute units (`amd.com/gpucores`) as node capacity and allocatable. Turning it off removes the entries again. |
| `dp.deviceMemoryScaling` | `-device_memory_scaling` | `1` | Multiplies the VRAM each GPU registers. Above `1`, pods get `HIP_OVERSUBSCRIBE=true` and `hipMalloc` is served from managed memory (needs XNACK on CDNA). |
| `dp.excludeGPUs` | `-exclude_gpus` | empty | GPUs this node keeps for itself, as PCI BDFs (`0000:06:00.0`) or DRM cards (`card1`). With none left, the stale registration is cleared. |
| `dp.hipLogLevel` | `LIBHIP_LOG_LEVEL` in pods | `0` | Log level of the memory hook in pods (1 error, 2 warn, 3 info, 4 debug); `0` keeps the library default. |
| `dp.cdi.enabled`, `dp.cdi.specDir` | `-cdi_spec_dir` | off, `/var/run/cdi` | Inject GPUs through CDI. |
| `dp.muslFailClosed.*` | `-musl_fail_closed`, `-ctr_path`, `-containerd_socket` | off | Refuse a slice whose image cannot load the memory hook (musl, static, or glibc older than 2.34). |
| | `-resource_naming_strategy` | `single` | `single` or `mixed`; see below. |
| `dp.hookInstaller.enabled` | | `true` | Copy `libamvgpu.so` from the image to `<dp.hostHookPath>/vgpu` on each node. |
| `dp.hostHookPath` | | `/usr/local` | Host directory for the hook. Workloads always see it at `/usr/local/vgpu/libamvgpu.so`. |
| `dp.operatingMode` | `OPERATING_MODE` env | `cu` | `cu` (whole GPUs sliced by CU) or `partition` (hard compute partitions). The `hami.io/amd-operating-mode` node annotation overrides it. |
| `dp.computePartition` | `COMPUTE_PARTITION` env | empty | In partition mode, the compute partition (`spx`/`dpx`/`qpx`/`cpx`) idle GPUs are switched to at startup. The `hami.io/amd-compute-partition` node annotation overrides it. |

## Operating Modes

The plugin registers AMD GPUs in one of two modes:

- **cu mode (default)**: each whole GPU registers as a soft device (default
  split count, `Mode` empty) and is sliced by CU and memory. This is the
  historical HAMi behavior.
- **partition mode**: devices register as hard compute partitions with
  `Mode` set to the partition type (`spx`, `qpx`) and `Count: 1`. The device
  ID is the same as in cu mode, so Pods allocated before a mode switch keep
  their accounting. The kubelet-facing device list publishes
  only the hard entries (no soft splits). Whole GPUs with `amdgpu_xcp_*`
  partitions in KFD topology are replaced by them; any other partitioned GPU
  registers as one hard device. GPUs without a compute partition type, and
  GPUs that cannot change it (SR-IOV VFs have no `available_compute_partition`
  file), stay soft.

Mode resolution, lowest to highest precedence:

1. Default `cu` when nothing is set.
2. `OPERATING_MODE` environment variable (chart value `dp.operatingMode`).
3. The `hami.io/amd-operating-mode` node annotation, which overrides the env
   per node (the NVIDIA `nvidia.com/mig.config` pattern).

The annotation is read once at plugin start; changing it requires a plugin
pod restart. The plugin logs the resolved mode as `operating mode: <mode>`.

### Changing the operating mode

Drain the node first. Pods placed under one mode keep that mode's slot count
and CU layout, so switching while GPU Pods run can leave the scheduler and the
node disagreeing about what is free. If a GPU switches while Pods still use
it, the plugin logs a warning and records a node Warning event,
`AMDGPUModeChangedInUse`, naming the Pods. It does not stop them.

1. Cordon and drain the node (`kubectl cordon <node>`,
   `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`), or
   delete every Pod that requests `amd.com/gpu`.
2. Change the mode: set `hami.io/amd-operating-mode` on the node, or the
   `dp.operatingMode` chart value.
3. Restart the plugin Pod (`kubectl -n kube-system rollout restart
   ds/amd-gpu-device-plugin-daemonset`).
4. Check the log for `operating mode: <mode>`, and that
   `amd.com/gpu` under `kubectl get node <node> -o jsonpath='{.status.allocatable}'`
   matches the new mode (the split count in `cu`; in `partition` one per
   partition-capable GPU, while GPUs that cannot change their partition, such
   as SR-IOV VFs, keep the split count).
5. `kubectl uncordon <node>`.

The same applies when upgrading from a build older than the device ID fix
(#159) that was running in `partition` mode: Pods allocated by that build are
recorded under a `<uuid>#<type>` ID the new build does not recognise. Drain
the node before upgrading.

Partition counts are per GPU: `amd-smi set -g <id> --compute-partition qpx`
flips a single GPU; mix spx and qpx GPUs on one node. See
[MI355X Partitioning](mi355x-partitioning.md) for kernel caveats.
## dmem VRAM cap

The memory hook limits VRAM inside the process, so it needs glibc 2.34 or newer and can be bypassed. The dmem cap is enforced by the kernel. It is on by default, and the plugin turns it off at startup, with one log line, unless the node has all of:

- cgroup v2 with the `dmem` controller (kernel 6.14 or newer, listed in `/sys/fs/cgroup/cgroup.controllers`; kernel 6.8 has none and the plugin logs that the cap is skipped),
- the systemd cgroup driver (`/sys/fs/cgroup/kubepods.slice` exists).

For each sliced GPU, the plugin writes `drm/<bdf>/vram <bytes>` to the pod cgroup's `dmem.max`. The cap is written in the background shortly after the container starts, because the pod cgroup can take a few seconds to appear, and lowering `dmem.max` below current usage does not reclaim VRAM, so memory a workload allocates before the cap lands stays allocated. A whole-GPU pod gets no dmem cap. Do not put whole-GPU pods and slices on the same card if VRAM isolation matters.

## musl fail-closed

`dp.muslFailClosed.enabled=true` makes the plugin inspect a sliced pod's image with `ctr` before it starts. If the image is musl-based, statically linked, or uses glibc older than 2.34, the memory hook would not load, so the pod is refused, unless dmem caps its VRAM anyway, in which case the plugin leaves the hook out. The containerd socket and data directory must be mounted with `mountPropagation: HostToContainer`. The chart's defaults are RKE2 paths; override `dp.muslFailClosed.ctrPath`, `containerdSocketDir`, `containerdSocket` and `containerdDataDir` for other distributions. This is experimental.

The chart defaults for `dp.muslFailClosed.ctrPath` and `dp.muslFailClosed.containerdDataDir` are RKE2 paths. On k3s set `ctrPath` to `/var/lib/rancher/k3s/data/current/bin/ctr` and `containerdDataDir` to `/var/lib/rancher/k3s/agent/containerd`.

The kubelet calls the plugin when it admits the pod, before it pulls the image. If the image is not on the node yet, the plugin starts pulling it in the background and refuses that first pod with `image is not on the node yet`; a retry (a Deployment recreates the pod) finds the image and starts.

## CDI

With `dp.cdi.enabled=true` the plugin writes an `amd.com/gpu` CDI spec to `dp.cdi.specDir`, with one device per DRM card plus `/dev/kfd`, and returns CDI device names instead of device nodes. The container runtime must have CDI enabled and read that directory; containerd 2.x and CRI-O do by default. CDI device responses from a device plugin need Kubernetes 1.28 or newer (the `DevicePluginCDIDevices` feature gate, on by default since 1.29).

## Resource naming strategy

This only matters for Instinct GPUs split into compute partitions (for example CPX):

- **`single`** (default): every GPU and partition is `amd.com/gpu`. Use it when all GPUs on a node use the same partition mode.
- **`mixed`**: partitions are named after their mode, for example `amd.com/cpx_nps4`, and unpartitioned GPUs stay `amd.com/gpu`. GPUs that cannot change their partition (SR-IOV VFs) count as unpartitioned. Use it when a node mixes partition modes.
