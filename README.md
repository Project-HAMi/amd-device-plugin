# HAMi AMD Device Plugin

[![CI](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/Project-HAMi/amd-device-plugin/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/Project-HAMi/amd-device-plugin/badge)](https://securityscorecards.dev/viewer/?uri=github.com/Project-HAMi/amd-device-plugin)
[![License](https://img.shields.io/github/license/Project-HAMi/amd-device-plugin)](LICENSE)

A Kubernetes device plugin that lets [HAMi](https://github.com/Project-HAMi/HAMi) share AMD GPUs between pods. It limits each pod to its share of the GPU's compute units and memory.

- [STATUS.md](STATUS.md): what was checked on a real GPU, the problems found and fixed, and what is left.
- [ONBOARDING.md](ONBOARDING.md): how the project works, for someone new.

The library that limits memory inside pods lives in [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core); `STATUS.md` and `ONBOARDING.md` cover it too. Install steps and settings are in [docs](docs/index.md), and the changes of each release in [CHANGELOG.md](CHANGELOG.md).

The same sharing through Kubernetes DRA is [amd-dra-driver](https://github.com/Project-HAMi/amd-dra-driver). A node runs one of the two: the chart keeps this plugin off nodes labelled `hami.io/amd-mode=dra`.
