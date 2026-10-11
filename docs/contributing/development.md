# Development Guidelines

## Getting Started

### Prerequisites

Before you begin development, ensure you have the following prerequisites:

- A working installation of Go (the version in `go.mod`, currently 1.26.9 or later).
- Access to a Kubernetes cluster for testing.
- Familiarity with Git and GitHub workflows.

### Cloning the Repository

To contribute to the project, start by cloning the repository:

```bash
git clone https://github.com/Project-HAMi/amd-device-plugin.git
cd amd-device-plugin
```

### Branching Strategy

When working on a new feature or bug fix, create a new branch from the `main` branch:

```bash
git checkout -b feature/my-new-feature
```

Make sure to name your branch descriptively to reflect the changes you are making.

## Development Workflow

1. **Make Changes**: Implement your changes in the codebase.
2. **Testing**: Ensure that your changes are covered by tests. The code uses cgo with AMD SMI and libdrm, so run the tests in the builder image as described in [ONBOARDING.md](https://github.com/Project-HAMi/amd-device-plugin/blob/main/ONBOARDING.md) (section 9, Testing). Add new tests as necessary.
3. **Commit Changes**: Commit your changes with a clear and concise commit message. Commits must be signed off (DCO), so use `-s`:

   ```bash
   git commit -s -m "Add feature X to improve Y"
   ```

4. **Push Changes**: Push your branch to the remote repository:

   ```bash
   git push origin feature/my-new-feature
   ```

5. **Create a Pull Request**: Navigate to the GitHub repository and create a pull request. Provide a detailed description of your changes and why they are necessary.

## Code Review Process

All contributions will undergo a code review process. Reviewers will assess the quality of the code, adherence to coding standards, and the completeness of tests. Be open to feedback and ready to make adjustments as needed.

## Hardware verification

Changes to discovery, AMD SMI, ROCr visibility, CU masks, the memory hook or the dmem cap need a run on a real GPU node. On an Instinct MI325X (one GPU, as a virtual function) with k3s and the HAMi scheduler these checks are the useful ones:

- `go vet ./...` and `go test -race ./...` in the builder image with `--privileged --device /dev/kfd --device /dev/dri -v /sys:/sys -v /sys/kernel/debug:/sys/kernel/debug`, so the GPU-gated tests run instead of skipping.
- A whole-GPU pod and a sliced pod (`amd.com/gpucores`, `amd.com/gpumem`): check `HSA_CU_MASK`, `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `LD_AUDIT` and `HIP_DEVICE_MEMORY_LIMIT_0`, then a `hipMalloc` over the limit.
- Several sliced pods at once: the masks must not overlap and the node lock annotation `hami.io/mutex.lock` must be empty afterwards.
- The dmem cap needs kernel 6.14 or newer: `dmem.capacity` must list `drm/<bdf>/vram` and a slice's pod cgroup must show it in `dmem.max`.
- `govulncheck ./...` should report no vulnerabilities.

Not covered by that node: several GPUs (XGMI peers, spread and binpack ordering, `amd.com/link-bind`), a real compute-partition flip (AMD SMI returns status 2 on a VF), RDNA behavior, and a `hipMalloc` larger than the VRAM (it needs XNACK).
