# Contributing

Welcome, and thank you for your interest in contributing to the HAMi
amd-device-plugin. This guide explains how to propose changes.

## Code of Conduct

This project follows the HAMi community
[Code of Conduct](https://github.com/Project-HAMi/community/blob/main/CODE-OF-CONDUCT.md).
By participating you are expected to uphold it.

## Getting Started

- Read the [README](README.md) for what the plugin does and how it is deployed.
- The plugin is written in Go and uses cgo against libdrm, hwloc and AMD SMI.
  The supported build is the container image:

  ```sh
  docker build -t amd-device-plugin:dev .
  ```

  For a local Go build you need `pkg-config`, `libdrm-dev`, `libhwloc-dev` and
  a ROCm install that provides `amd_smi`, then:

  ```sh
  go build ./...
  go test ./...
  ```

## Finding Something to Work On

- Browse the [open issues](https://github.com/Project-HAMi/amd-device-plugin/issues).
- Issues labeled `good first issue` are a good starting point.
- If you plan to work on something, comment on the issue so others know.

## Filing an Issue

Open a [new issue](https://github.com/Project-HAMi/amd-device-plugin/issues/new/choose)
and include: what you expected, what happened, the GPU model and ROCm version,
and steps to reproduce. For security problems, do not open a public issue;
follow [SECURITY.md](SECURITY.md).

## Pull Request Workflow

1. Fork the repository and create a topic branch from `main`.
2. Make your change. Keep pull requests focused and small where possible.
3. Add or update tests. Run `go build ./...`, `go vet ./...` and `go test ./...`.
4. Sign off every commit to certify the [DCO](https://developercertificate.org/):

   ```sh
   git commit -s -m "your message"
   ```

5. Use the pull request template and add a `/kind` label
   (`/kind bug`, `/kind feature`, `/kind documentation`).
6. Open the pull request against `main` and describe what changed and why.
   Link the issue it fixes with `Fixes #<issue>`.

## Code Review

- Maintainers review pull requests and may request changes.
- Automated checks (CI, DCO, CodeQL) must pass.
- Once a reviewer is satisfied they add `/lgtm`, and an approver adds
  `/approve`; the bot then merges the pull request.

Thank you for helping improve the project.
