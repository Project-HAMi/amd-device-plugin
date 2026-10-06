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
| `dp.cdi.enabled`, `dp.cdi.specDir` | `-cdi_spec_dir` | off, `/var/run/cdi` | Inject GPUs through CDI. |
| `dp.muslFailClosed.*` | `-musl_fail_closed`, `-ctr_path`, `-containerd_socket` | off | Refuse a slice whose image cannot load the memory hook (musl, static, or glibc older than 2.34). |
| | `-resource_naming_strategy` | `single` | `single` or `mixed`; see below. |
| `dp.hookInstaller.enabled` | | `true` | Copy `libamvgpu.so` from the image to `<dp.hostHookPath>/vgpu` on each node. |
| `dp.hostHookPath` | | `/usr/local` | Host directory for the hook. Workloads always see it at `/usr/local/vgpu/libamvgpu.so`. |

## dmem VRAM cap

The memory hook limits VRAM inside the process, so it needs glibc 2.34 or newer and can be bypassed. The dmem cap is enforced by the kernel. It is on by default, and the plugin turns it off at startup, with one log line, unless the node has all of:

- cgroup v2 with the `dmem` controller (a recent kernel, listed in `/sys/fs/cgroup/cgroup.controllers`),
- the systemd cgroup driver (`/sys/fs/cgroup/kubepods.slice` exists).

For each sliced GPU, the plugin writes `drm/<bdf>/vram <bytes>` to the pod cgroup's `dmem.max`. The cap is written in the background shortly after the container starts, because the pod cgroup can take a few seconds to appear, and lowering `dmem.max` below current usage does not reclaim VRAM, so memory a workload allocates before the cap lands stays allocated. A whole-GPU pod gets no dmem cap. Do not put whole-GPU pods and slices on the same card if VRAM isolation matters.

## musl fail-closed

`dp.muslFailClosed.enabled=true` makes the plugin inspect a sliced pod's image with `ctr` before it starts. If the image is musl-based, statically linked, or uses glibc older than 2.34, the memory hook would not load, so the pod is refused, unless dmem caps its VRAM anyway, in which case the plugin leaves the hook out. The containerd socket and data directory must be mounted with `mountPropagation: HostToContainer`. The chart's defaults are RKE2 paths; override `dp.muslFailClosed.ctrPath`, `containerdSocketDir`, `containerdSocket` and `containerdDataDir` for other distributions. This is experimental.

## CDI

With `dp.cdi.enabled=true` the plugin writes an `amd.com/gpu` CDI spec to `dp.cdi.specDir`, with one device per DRM card plus `/dev/kfd`, and returns CDI device names instead of device nodes. The container runtime must have CDI enabled and read that directory; containerd 2.x and CRI-O do by default. CDI device responses from a device plugin need Kubernetes 1.28 or newer (the `DevicePluginCDIDevices` feature gate, on by default since 1.29).

## Resource naming strategy

This only matters for Instinct GPUs split into compute partitions (for example CPX):

- **`single`** (default): every GPU and partition is `amd.com/gpu`. Use it when all GPUs on a node use the same partition mode.
- **`mixed`**: partitions are named after their mode, for example `amd.com/cpx_nps4`, and unpartitioned GPUs stay `amd.com/gpu`. Use it when a node mixes partition modes.
