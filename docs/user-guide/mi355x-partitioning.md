# MI355X (gfx950) Partitioning

The MI355X splits into hardware compute partitions with `amd-smi set`:

| Profile | Partitions per GPU | CUs per partition | Mode suffix |
|---------|--------------------|-------------------|-------------|
| spx     | 1                  | 256               | `#spx`      |
| dpx     | 2                  | 128               | `#dpx`      |
| qpx     | 4                  | 64                | `#qpx`      |
| cpx     | 8                  | 32                | `#cpx`      |

Each GPU has 256 CUs and 294896 MiB of VRAM (288 GB). Partition mode
requires the plugin's `partition` operating mode; see
[Configuration](configuration.md#operating-modes).

## Registration by kernel

Whether the plugin registers whole GPUs or `amdgpu_xcp_*` partitions depends
on the kernel's KFD topology, not on the plugin:

- **Kernels whose KFD topology exposes XCP render minors** (the platform
  devices under `/sys/devices/platform/amdgpu_xcp_*` have `renderD` minors
  listed in `/sys/class/kfd/kfd/topology/nodes`): each partition registers
  as a hard `#<mode>` device with an even share of VRAM and CUs (qpx: 4
  devices of 73728 MiB and 64 CUs per GPU). This is the full XCP path.
- **Kernels without XCP render minors in KFD topology** (observed on
  kernel 6.8.0-136 in spx mode): the plugin's KFD-validity gates drop every
  `amdgpu_xcp_*` entry. Only the 8 whole-GPU nodes register. Note that the
  KFD topology changes after a hardware partition flip: on 6.8.0-136, after
  the plugin flips a GPU to qpx, the XGMI reset re-probes KFD and 3 of the 4
  partition nodes per GPU appear (21 XCP entries for 7 qpx GPUs, verified
  live). The missing 4th node is dropped by the same render-minor gate.
  Additionally, the partitions of one GPU share a single ROCr unique_id on
  this kernel, so the scheduler sees 3 devices with one ID and can only
  allocate one partition per GPU safely; distinct per-partition unique_ids
  need a kernel/driver that reports them.

## Known kernel caveat: 6.8.0-136

> On 6.8.0-136 the KFD topology lacks XCP render minors, so QPX/DPX nodes
> register one hard whole-GPU `#qpx` entry per GPU (full 256 CU / 294896 MiB
> capacity), not 4 XCP partitions. The registration and capacity math are
> ready for XCPs on a kernel that exposes them; until then a QPX pod
> requesting 64 cores / 73728 MiB still pins one GPU and gets memory- and
> CU-limited correctly.

The capacity divisor for XCP entries comes from the AMD SMI partition
profile's `NumPartitions` for the current compute type, not from counting
`amdgpu_xcp_*` children (gfx950 exposes 7 XCD chiplets per GPU regardless of
partition mode, so child counts are wrong divisors).

## Verifying on a node

```bash
# KFD topology node count and simd counts; XCP partitions appear as nodes
# with simd_count 256 (qpx) and a render node in the same topology.
cat /sys/class/kfd/kfd/topology/nodes/*/properties | grep -E "simd_count|gfx_target_version"

# Platform partition devices and their render minors:
ls /sys/devices/platform/amdgpu_xcp_*/drm/

# Current compute partition per GPU:
amd-smi static -g 0 | grep "Compute Part"

# Flip one GPU to qpx (per GPU, not per node):
amd-smi set -g 0 --compute-partition qpx
```

After a plugin restart, the node annotation
`hami.io/node-amd-register` shows the registered devices: soft entries
(`Count: 10`, empty `Mode`) in cu mode, hard entries (`Count: 1`, `Mode:
qpx`, `#qpx` suffix) in partition mode. The plugin log reports
`operating mode: <mode>` and `ListAndWatch: sending N split devices`.
