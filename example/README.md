# Examples

Workloads that run on AMD GPUs managed by HAMi and this device plugin.

| File | What it shows |
|---|---|
| `pod/pytorch-slice.yaml` | A quarter of one GPU's CUs and 4 GiB of VRAM (`amd.com/gpucores`, `amd.com/gpumem`) |
| `pod/pytorch.yaml` | One whole GPU |
| `pod/pytorch-non-privileged.yaml` | One whole GPU without a privileged container |
| `pod/jax-multi-gpu.yaml`, `pod/jax-non-privileged.yaml` | Two whole GPUs with JAX |
| `pod/tensorflow-gpu.yaml` | One whole GPU with TensorFlow |
| `vllm-serve/` | A vLLM inference deployment and service |

Apply one with `kubectl apply -f <file>` and read its output with `kubectl logs <pod>`.
