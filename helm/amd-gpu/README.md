# AMD GPU Helm Chart

![Version: 0.0.3](https://img.shields.io/badge/Version-0.0.3-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.3](https://img.shields.io/badge/AppVersion-0.0.3-informational?style=flat-square)

HAMi AMD device plugin for fractional GPU allocation on Kubernetes

## Requirements

Kubernetes: `>= 1.19.0`, and the HAMi scheduler, which reads the node annotation this plugin publishes.

## Install

```bash
helm install amd-gpu ./helm/amd-gpu -n kube-system
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| dp.image.repository | string | `"ghcr.io/project-hami/amd-device-plugin"` | Device plugin image. |
| dp.image.tag | string | `""` | Image tag; empty uses the chart `appVersion`. |
| dp.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| dp.serviceAccount.create | bool | `true` | Create the service account and RBAC needed to register devices and persist allocations. |
| dp.serviceAccount.name | string | `""` | Override the service account name. With `create: false`, RBAC is bound to this existing account, or skipped when it is empty. |
| dp.serviceAccount.annotations | object | `{}` | Service account annotations. |
| dp.securityContext.privileged | bool | `true` | Let the plugin query AMD GPU device nodes on the host. |
| dp.securityContext.capabilities.drop | list | `["ALL"]` | Capabilities dropped from the plugin container. |
| dp.hostHookPath | string | `"/usr/local"` | Host path prefix; the hook is installed under `<hostHookPath>/vgpu`. |
| dp.hookInstaller.enabled | bool | `true` | Install the image-bundled `libamvgpu.so` on the node with a postStart hook. |
| dp.logVerbosity | int | `2` | glog verbosity; 4 adds topology parsing, 5 adds annotation decoding. |
| dp.healthPulse | int | `10` | Seconds between per-GPU health checks; 0 disables them. |
| dp.reportNodeCapacity | bool | `false` | Publish the healthy GPUs' memory (`amd.com/gpumem`, MiB) and compute units (`amd.com/gpucores`) as node capacity and allocatable. |
| dp.splitCount | int | `0` | How many workloads may share one GPU. `0` picks it per GPU: 2 on gfx12, otherwise 10. |
| dp.allocatorPolicy | string | `"besteffort"` | Multi-GPU preferred allocation: `besteffort` (same as `binpack`) or `spread`. |
| dp.dmemBackend | bool | `true` | Also cap sliced VRAM with the kernel dmem cgroup; skipped on nodes without dmem or the systemd cgroup driver. |
| dp.muslFailClosed.enabled | bool | `false` | Refuse a slice whose image cannot load the memory hook (musl, static, or glibc older than 2.34), unless dmem caps it. Experimental. |
| dp.muslFailClosed.ctrPath | string | `"/var/lib/rancher/rke2/bin/ctr"` | Host path of `ctr`. |
| dp.muslFailClosed.containerdSocketDir | string | `"/run/k3s/containerd"` | Host directory of the containerd socket. |
| dp.muslFailClosed.containerdSocket | string | `"/run/k3s/containerd/containerd.sock"` | containerd socket path. |
| dp.muslFailClosed.containerdDataDir | string | `"/var/lib/rancher/rke2/agent/containerd"` | containerd data directory. |
| dp.excludeGPUs | list | `[]` | GPUs this node keeps for itself and does not register, as PCI BDFs (`0000:06:00.0`) or DRM cards (`card1`). |
| dp.hipLogLevel | int | `0` | `LIBHIP_LOG_LEVEL` of the pods that load `libamvgpu` (1 error, 2 warn, 3 info, 4 debug); 0 keeps the library default. |
| dp.cdi.enabled | bool | `false` | Inject GPUs through CDI instead of device nodes. |
| dp.cdi.specDir | string | `"/var/run/cdi"` | Host directory the CDI spec is written to; the runtime must read it. |
| dp.resources | object | `{}` | Plugin container resources. |
| dp.updateStrategy | object | `RollingUpdate, maxUnavailable: 1` | DaemonSet update strategy. |
| monitor.enabled | bool | `false` | Run the metrics DaemonSet: per-container VRAM from the dmem cgroup controller and host memory, load, temperature and power from sysfs, under the metric names of the HAMi NVIDIA vGPUmonitor. Needs the systemd cgroup driver on a kernel with the dmem controller. |
| monitor.metricsBindAddress | string | `":9394"` | Address the monitor serves `/metrics` on; the container port and the scrape annotation follow it. |
| monitor.resources | object | `{}` | Monitor container resources. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| tolerations | list | `[{key: CriticalAddonsOnly, operator: Exists}]` | DaemonSet tolerations. |
| node_selector_enabled | bool | `false` | Restrict the DaemonSet to `node_selector`. |
| node_selector | object | `pci-0300_1002.present: "true"`, `kubernetes.io/arch: amd64` | Node selector used when enabled. |

## More information

https://github.com/Project-HAMi/amd-device-plugin
