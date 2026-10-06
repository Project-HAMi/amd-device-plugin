# AMD GPU Helm Chart

![Version: 0.0.1](https://img.shields.io/badge/Version-0.0.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

HAMi AMD device plugin for fractional GPU allocation on Kubernetes

## Requirements

Kubernetes: `>= 1.18.0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| dp.image.repository | string | `"ghcr.io/project-hami/amd-device-plugin"` | Device plugin image. |
| dp.image.tag | string | `"0.0.1"` | Image tag. |
| dp.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| dp.serviceAccount.create | bool | `true` | Create the service account and RBAC needed to register devices and persist allocations. |
| dp.serviceAccount.name | string | `""` | Override the service account name. |
| dp.serviceAccount.annotations | object | `{}` | Service account annotations. |
| dp.securityContext.privileged | bool | `true` | Let the plugin query AMD GPU device nodes on the host. |
| dp.securityContext.capabilities.drop | list | `["ALL"]` | Capabilities dropped from the plugin container. |
| dp.hostHookPath | string | `"/usr/local"` | Host path prefix; the hook is installed under `<hostHookPath>/vgpu`. |
| dp.hookInstaller.enabled | bool | `true` | Install the image-bundled `libamvgpu.so` on the node with a postStart hook. |
| dp.healthPulse | int | `10` | Seconds between per-GPU health checks; 0 disables them. |
| dp.splitCount | int | `0` | How many workloads may share one GPU. `0` picks it per GPU: 2 on gfx12, otherwise 10. |
| dp.allocatorPolicy | string | `"besteffort"` | Multi-GPU preferred allocation: `besteffort` (same as `binpack`) or `spread`. |
| dp.dmemBackend | bool | `true` | Also cap sliced VRAM with the kernel dmem cgroup; skipped on nodes without dmem or the systemd cgroup driver. |
| dp.muslFailClosed.enabled | bool | `false` | Refuse a slice whose image cannot load the memory hook (musl or static). Experimental. |
| dp.muslFailClosed.ctrPath | string | `"/var/lib/rancher/rke2/bin/ctr"` | Host path of `ctr`. |
| dp.muslFailClosed.containerdSocketDir | string | `"/run/k3s/containerd"` | Host directory of the containerd socket. |
| dp.muslFailClosed.containerdSocket | string | `"/run/k3s/containerd/containerd.sock"` | containerd socket path. |
| dp.muslFailClosed.containerdDataDir | string | `"/var/lib/rancher/rke2/agent/containerd"` | containerd data directory. |
| dp.cdi.enabled | bool | `false` | Inject GPUs through CDI instead of device nodes. |
| dp.cdi.specDir | string | `"/var/run/cdi"` | Host directory the CDI spec is written to; the runtime must read it. |
| dp.resources | object | `{}` | Plugin container resources. |
| dp.updateStrategy | object | `RollingUpdate, maxUnavailable: 1` | DaemonSet update strategy. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| tolerations | list | `[{key: CriticalAddonsOnly, operator: Exists}]` | DaemonSet tolerations. |
| node_selector_enabled | bool | `false` | Restrict the DaemonSet to `node_selector`. |
| node_selector | object | `pci-0300_1002.present: "true"`, `kubernetes.io/arch: amd64` | Node selector used when enabled. |

## More information

https://github.com/Project-HAMi/amd-device-plugin
