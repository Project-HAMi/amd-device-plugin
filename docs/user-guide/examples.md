# Examples

The files are in [example/](https://github.com/Project-HAMi/amd-device-plugin/tree/main/example). HAMi's admission webhook routes any pod that requests `amd.com/*` resources to the HAMi scheduler.

## A slice of one GPU

25% of one GPU's compute units and 4 GiB of its VRAM ([pytorch-slice.yaml](https://github.com/Project-HAMi/amd-device-plugin/blob/main/example/pod/pytorch-slice.yaml)):

```yaml
resources:
  limits:
    amd.com/gpu: 1
    amd.com/gpucores: 25
    amd.com/gpumem: 4096
```

Inside the container, `HSA_CU_MASK` holds the CU slice and `HIP_DEVICE_MEMORY_LIMIT_0` the VRAM limit, and the `libamvgpu.so` hook enforces both:

```bash
kubectl exec <pod> -- sh -c 'echo $HSA_CU_MASK $HIP_DEVICE_MEMORY_LIMIT_0'
# for example, on a 64-CU GPU: 0:0-15 4096m
```

## One whole GPU

Leave out `amd.com/gpucores` and `amd.com/gpumem` ([pytorch.yaml](https://github.com/Project-HAMi/amd-device-plugin/blob/main/example/pod/pytorch.yaml)). A whole-GPU pod gets no hook, so any image works:

```yaml
resources:
  limits:
    amd.com/gpu: 1
```

## Several GPUs

`amd.com/gpu: 2` gives two GPUs ([jax-multi-gpu.yaml](https://github.com/Project-HAMi/amd-device-plugin/blob/main/example/pod/jax-multi-gpu.yaml)). With `amd.com/gpucores` and `amd.com/gpumem` each GPU gets the same share. `ROCR_VISIBLE_DEVICES`, `HSA_CU_MASK` and `HIP_DEVICE_MEMORY_LIMIT_<i>` follow the same container-local order, so index `i` is the same GPU in all three.

## Non-privileged containers

[pytorch-non-privileged.yaml](https://github.com/Project-HAMi/amd-device-plugin/blob/main/example/pod/pytorch-non-privileged.yaml) runs without `privileged`. The device plugin hands the container its GPU device nodes; this ROCm workload also sets `hostIPC: true` and an `Unconfined` seccomp profile:

```yaml
hostIPC: true
containers:
- securityContext:
    privileged: false
    allowPrivilegeEscalation: false
    seccompProfile:
      type: Unconfined
```
