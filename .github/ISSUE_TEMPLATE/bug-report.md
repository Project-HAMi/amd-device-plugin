---
name: Bug Report
about: Report a problem encountered while using amd-device-plugin
labels: bug
---

<!-- Please use this template while reporting a bug and provide as much info as possible. Not doing so may result in your bug not being addressed in a timely manner. Thanks!
-->

**What happened**:

**What you expected to happen**:

**How to reproduce it (as minimally and precisely as possible)**:

**Anything else we need to know?**:

- Relevant excerpts from AMD SMI or ROCm SMI output; mask GPU UUIDs, PCI addresses, and host or node identifiers
- Relevant Docker or containerd configuration sections. Omit credentials, tokens, passwords, private keys, certificates, and unrelated host data.
- Relevant, time-bounded excerpts from the amd-device-plugin container and hook-installer logs
- Relevant, time-bounded excerpts from the HAMi scheduler container logs
- Relevant, time-bounded excerpts from the kubelet logs on the node (e.g: `sudo journalctl -r -u kubelet`)
- Relevant, redacted excerpts from the `hami.io/node-amd-register` annotation
- The relevant Helm values, ConfigMaps, or deployment manifests
- Relevant, time-bounded kernel output lines from `dmesg`

Before posting, remove or mask credentials, tokens, GPU identifiers, PCI addresses, node or host identifiers, and other sensitive data from configuration and logs.

**Environment**:
- amd-device-plugin version or image:
- AMD GPU model:
- ROCm and AMD SMI version:
- `amdgpu` kernel driver version:
- Kubernetes and HAMi version:
- Docker or containerd version:
- Helm chart version and relevant configuration:
- Workload image and libc version, if memory isolation is involved:
- Kernel version from `uname -a`:
- Others:
