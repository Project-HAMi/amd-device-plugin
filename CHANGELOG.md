# Changelog

## v0.0.1

Initial Project-HAMi release of the AMD device plugin.

- Reports AMD SMI UUIDs and product names in HAMi device annotations.
- Reports PCI BDF through `custominfo.pciBDF` and physical VRAM/CU capacity through `libdrm_amdgpu`.
- Applies ROCr device visibility, CU masks, and fractional-memory limits during allocation.
- Persists per-Pod CU allocation state across device-plugin restarts.
- Packages the temporary `libamvgpu.so` hook and installs it on each node with a HAMi-style `postStart` lifecycle hook.
- Ships Helm chart `0.0.1` with the required ServiceAccount, RBAC, host hook delivery, and AMD device access configuration.

Known limitation: the bundled memory hook requires glibc symbols through `GLIBC_2.34`; see the README for workload-image compatibility details.
