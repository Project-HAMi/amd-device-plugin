# Resource Allocation

## Who decides what

1. **The HAMi scheduler** picks the node and the GPUs. It uses the shares (`count`), VRAM and CUs that each GPU publishes in `hami.io/node-amd-register`, and writes its choice to the pod's annotations.
2. **The device plugin**, in kubelet's `Allocate` call, turns that choice into the container's device nodes and environment: `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `HSA_CU_MASK` and `HIP_DEVICE_MEMORY_LIMIT_<i>`. It records which CUs each pod holds in the pod's `hami.io/amd-cu-allocated` annotation, so it can rebuild them after a plugin restart.
3. **The `libamvgpu.so` hook** and, where available, the dmem cgroup enforce the VRAM limit inside the container. ROCm enforces the CU mask.

## Shares per GPU

Each GPU is published `count` times; that count is how many pods may share it. By default it is 2 on gfx12 GPUs and 10 on others; `dp.splitCount` overrides it for every GPU. gfx12 has only 2 hardware pipes for user compute queues, so more than about 2 processes on one GPU can lose most of their throughput, independent of their CU and memory slices. How much depends on the model and firmware: an Ollama qwen2.5:3b pair lost about 83%, while a plain kernel loop split the card evenly across 6 processes with no loss in total.

## Compute units

`amd.com/gpucores: N` asks for N% of a GPU's CUs. The plugin hands out free CUs from the lowest index up, and remembers them so slices never overlap.

On RDNA (gfx10 and later) the CU mask is applied per WGP, a pair of CUs, and a mask that enables only one CU of a pair is ignored, which would run the pod on the whole GPU. So on RDNA the plugin allocates whole WGPs and rounds the request up. For example, 3 CUs become 4 (`0:0-3`), and the next slice starts at `0:4`. The plugin publishes the pair size as `custominfo.cuPerWGP`, so a HAMi scheduler that reads it accounts the same count. A single-WGP GPU, such as a small iGPU, can only be given out whole.

The mask is cooperative. The hook pins `HSA_CU_MASK` to the pod spec so the process cannot widen it, but a process that re-executes itself without the hook runs outside the slice.

## Memory

`amd.com/gpumem: M` limits VRAM to M MiB per GPU, through `HIP_DEVICE_MEMORY_LIMIT_<i>` for the hook and `dmem.max` for the kernel cap. See [Configuration](configuration.md) for when dmem applies.

## Picking GPUs inside a node

When kubelet itself chooses among a node's devices (`GetPreferredAllocation`), the plugin ranks GPU combinations with `dp.allocatorPolicy`:

- **`besteffort`** (default) and **`binpack`** (the same policy): prefer the closest GPUs, meaning the same NUMA node and the fastest links (XGMI, then PCIe), and keep partitions of one GPU together.
- **`spread`**: prefers the farthest GPUs, spreading across NUMA nodes and links.

GPUs with no direct GPU-to-GPU link, as on most consumer cards and passthrough VMs, are treated as connected through the host.
