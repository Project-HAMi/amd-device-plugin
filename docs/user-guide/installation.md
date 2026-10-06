# Installation

## Prerequisites

- Linux amd64 nodes with ROCm-supported AMD GPUs, the `amdgpu` kernel driver, `/dev/kfd` and `/dev/dri`. See the [ROCm installation guide](https://rocm.docs.amd.com/projects/install-on-linux/en/latest/) for drivers.
- Kubernetes with the [HAMi scheduler](https://github.com/Project-HAMi/HAMi) installed. HAMi schedules `amd.com/gpu`, `amd.com/gpucores` and `amd.com/gpumem` out of the box. Install it with its own device plugin disabled if the cluster has no NVIDIA GPUs:

  ```bash
  helm repo add hami-charts https://project-hami.github.io/HAMi/
  helm install hami hami-charts/hami -n kube-system --set devicePlugin.enabled=false
  ```

- Remove any other plugin that registers `amd.com/gpu` on the same nodes, such as the upstream ROCm device plugin. Kubelet cannot serve one resource from two plugins.

## Install the device plugin

```bash
helm upgrade --install amd-gpu ./helm/amd-gpu --namespace kube-system --create-namespace
```

The chart creates the DaemonSet, the ServiceAccount and RBAC the plugin needs to write node and pod annotations, and a `postStart` hook that copies the `libamvgpu.so` memory hook to `/usr/local/vgpu` on each node. See [Configuration](configuration.md) for the values.

## Verify

```bash
kubectl -n kube-system get pods -l name=amd-gpu-dp-ds
kubectl get node <node> -o jsonpath='{.status.allocatable.amd\.com/gpu}'
kubectl get node <node> -o jsonpath='{.metadata.annotations.hami\.io/node-amd-register}'
```

`amd.com/gpu` is the number of GPUs times their share count: by default 2 per gfx12 GPU and 10 per other GPU. The annotation lists each GPU's UUID, product name, VRAM and CU count, plus `custominfo` with the PCI BDF, compute-queue count and `cuPerWGP`.

## Optional components

- **GPU health from the AMD Device Metrics Exporter.** Install the [exporter](https://github.com/ROCm/device-metrics-exporter) with its gRPC socket enabled at `/var/lib/amd-metrics-exporter/`. The plugin then also uses the per-GPU health the exporter reports, for example after ECC errors. Without it, the plugin still checks that each GPU's device node can be opened.
- **Node labeller.** `k8s-ds-amdgpu-labeller.yaml` deploys the upstream labeller, which adds `amd.com/gpu.*` node labels such as VRAM, CU count, device ID and family:

  ```bash
  kubectl apply -f k8s-ds-amdgpu-labeller.yaml
  ```

## Troubleshooting

- **No `amd.com/gpu` on the node.** Read the plugin log with `kubectl -n kube-system logs ds/amd-gpu-device-plugin-daemonset`. Check that `/dev/kfd` and `/dev/dri` exist on the host and that no other plugin owns `amd.com/gpu`.
- **A sliced pod fails with `UnexpectedAdmissionError`.** The GPU has no free CUs for the request, often because of slices rounded up to whole WGPs on RDNA. A HAMi scheduler that reads `cuPerWGP` keeps such pods `Pending` instead.
- **A sliced pod ignores its memory limit.** The image needs glibc 2.34 or newer for the hook. Use a newer image, or enable dmem (see [Configuration](configuration.md)).

## Uninstall

```bash
helm uninstall amd-gpu -n kube-system
```
