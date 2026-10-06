# HAMi AMD Device Plugin

The Kubernetes device plugin that lets [HAMi](https://github.com/Project-HAMi/HAMi) share AMD GPUs between pods. It registers AMD GPUs with HAMi and, when a pod starts, limits it to the compute units (CUs) and VRAM HAMi gave it.

- [Installation](user-guide/installation.md): install with Helm and verify.
- [Configuration](user-guide/configuration.md): Helm values, plugin flags and resource names.
- [Examples](user-guide/examples.md): whole-GPU, sliced and multi-GPU pods.
- [Resource allocation](user-guide/resource-allocation.md): how GPUs, CUs and VRAM are handed out, and the limits of slicing.
- [Development](contributing/development.md): build, test and contribute.

The [README](https://github.com/Project-HAMi/amd-device-plugin/blob/main/README.md) has the short version of all of this.
