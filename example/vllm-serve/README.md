# Serve a model with vLLM

This example shows how to serve a model with vLLM on an AMD GPU managed by HAMi and this device plugin.

## Install HAMi and the device plugin

Follow the [installation guide](../../docs/user-guide/installation.md): install the HAMi scheduler, then the device plugin with its Helm chart.

## Prepare the manifests

This folder holds the three manifests:

- [hf_token.yaml](hf_token.yaml): a Secret with your Hugging Face token. It is only needed for gated models; skip it otherwise. Replace the `token` value with your token encoded in base64:

    ```bash
    echo -n '<your HF TOKEN>' | base64
    ```

- [deployment.yaml](deployment.yaml): the vLLM Deployment. It requests one GPU (`amd.com/gpu: "1"`) and serves `mistralai/Mistral-7B-v0.3` on port 8888. That model needs a GPU with more than 16 GB of VRAM; on a smaller card change the model in `args`.
- [service.yaml](service.yaml): a ClusterIP Service on port 80 that forwards to the Deployment.

## Launch the pods

```bash
kubectl apply -f hf_token.yaml
kubectl apply -f deployment.yaml
kubectl apply -f service.yaml
```

## Test the service

Get the CLUSTER-IP of the `mistral-7b` Service:

```bash
kubectl get svc mistral-7b
```

List the models (use the real CLUSTER-IP of your environment):

```bash
curl http://<CLUSTER-IP>:80/v1/models
```

Send a request:

```bash
curl http://<CLUSTER-IP>:80/v1/completions -H "Content-Type: application/json" -d '{
    "model": "mistralai/Mistral-7B-v0.3",
    "prompt": "San Francisco is a",
    "max_tokens": 7,
    "temperature": 0
  }'
```
