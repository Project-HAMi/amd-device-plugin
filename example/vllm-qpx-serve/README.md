# vLLM serving on QPX hard partitions (MI355X)

QPX splits each MI355X GPU into 4 compute partitions of 64 CUs and
73728 MiB (288 GB / 4). Pods request a whole partition by asking for
the full per-partition memory and core count:

- `amd.com/gpu: 1` (one partition)
- `amd.com/gpumem: 73728` (MiB, one QPX partition)
- `amd.com/gpucores: 64` (CUs, one QPX partition)

## Prerequisites

1. Device plugin in `partition` mode with QPX as the target partition.
   The plugin flips the GPUs itself through the AMD SMI API at startup;
   no manual `amd-smi set` on the node is needed. Either set the chart
   values (`dp.operatingMode: partition`, `dp.computePartition: qpx`),
   or per node:

   ```
   kubectl annotate node <gpu-node> hami.io/amd-operating-mode=partition
   kubectl annotate node <gpu-node> hami.io/amd-compute-partition=qpx
   kubectl delete pod -n kube-system -l ... # restart the device-plugin pod
   ```

   The annotations override the chart values on that node (the NVIDIA
   `nvidia.com/mig.config`-style per-node switch). GPUs with running
   workloads fail to flip and keep their current mode; the plugin logs
   each per-GPU result and registers whatever mode the hardware is in.

2. HAMi scheduler device config with the `amd` section (see
   device-config.yaml). On kernels whose KFD topology does not expose
   amdgpu_xcp_ nodes (e.g. 6.8.0-136), each GPU registers one hard
   `#qpx` device instead of 4 XCP partitions; requesting a full
   partition still pins one GPU and limits memory and CUs correctly.

## Deploy

```
kubectl apply -f hf_token.yaml
kubectl apply -f deployment.yaml
kubectl apply -f service.yaml
```

## Test

```
kubectl get svc
curl http://<CLUSTER-IP>:80/v1/models
curl http://<CLUSTER-IP>:80/v1/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "mistralai/Mistral-7B-v0.3", "prompt": "San Francisco is a", "max_tokens": 7, "temperature": 0}'
```
