# HAMi AMD Device Plugin

The HAMi AMD Device Plugin discovers AMD GPUs, reports hardware identity through HAMi node annotations, and enforces HAMi fractional GPU allocations in Kubernetes clusters.

> The top-level [README](../README.md) and Helm chart are the canonical deployment documentation for the HAMi fork. Some detailed pages below describe inherited upstream ROCm functionality and examples.

## Features

- Implements the Kubernetes Device Plugin API for AMD GPUs
- Exposes AMD GPUs as `amd.com/gpu` resources in Kubernetes
- Reports AMD SMI UUID, product type, PCI BDF, VRAM, and compute units to HAMi
- Enables fine-grained GPU allocation for containers

## System Requirements

- **Kubernetes**: v1.18 or higher
- **AMD GPUs**: ROCm-capable AMD GPU hardware
- **GPU Drivers**: AMD GPU drivers or ROCm stack installed on worker nodes

See the [ROCm System Requirements](https://rocm.docs.amd.com/projects/install-on-linux/en/latest/reference/system-requirements.html) for detailed hardware compatibility information.

## Quick Start

The supported `v0.0.1` deployment is the repository Helm chart. It installs the ServiceAccount/RBAC required by HAMi annotations and delivers the bundled memory hook to the host.

Create a DaemonSet in your Kubernetes cluster with the following command:

```bash
helm upgrade --install amd-gpu ./helm/amd-gpu \
  --namespace kube-system \
  --create-namespace
```

### Deploy the Node Labeler (Optional)

For enhanced GPU discovery and scheduling, deploy the AMD GPU Node Labeler:

```bash
kubectl create -f k8s-ds-amdgpu-labeller.yaml
```

This will automatically label nodes with GPU-specific information such as VRAM size, compute units, and device IDs.

### Verify Installation

After deploying the device plugin, verify that your AMD GPUs are properly recognized as schedulable resources:

```bash
# List all nodes with their AMD GPU capacity
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:"status.capacity.amd\.com/gpu"

NAME             GPU
k8s-node-01      8
```

## Example Workload

You can restrict workloads to a node with a GPU by adding `resources.limits` to the pod definition. An example pod definition is provided in [example/pod/pytorch.yaml](https://raw.githubusercontent.com/Project-HAMi/amd-device-plugin/main/example/pod/pytorch.yaml). Create the pod by running:

```bash
kubectl create -f https://raw.githubusercontent.com/Project-HAMi/amd-device-plugin/main/example/pod/pytorch.yaml
```

Check the pod status with:

```bash
kubectl describe pods
```

After the pod is running, view the benchmark results with:

```bash
kubectl pytorch-gpu-pod-example
```

## Contributing

We welcome contributions to this project! Please refer to the [Development Guidelines](contributing/development.md) for details on how to get involved.
