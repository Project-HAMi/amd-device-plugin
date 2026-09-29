# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 0.0.x   | ✅ Security fixes   |

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly. Do
**not** open a public issue for security problems.

### How to Report
- **GitHub Security Advisories**: [submit a private report](https://github.com/Project-HAMi/amd-device-plugin/security/advisories/new).
- **Bug Bounty**: this project does not currently offer a public bug bounty program.

### Information to Include
- A clear and concise description of the vulnerability.
- Steps to reproduce the issue.
- Any potential attack scenario or security impact.
- Suggested mitigations or fixes, if available.

## Is It In Scope?

This device plugin registers AMD GPUs to Kubernetes and applies HAMi soft vGPU
limits: memory through `HIP_DEVICE_MEMORY_LIMIT` and the `libamvgpu.so`
`LD_AUDIT` hook, and compute through `HSA_CU_MASK`. These limits are for
cooperative multi-tenant sharing on a trusted cluster. They are not a hard
security boundary against a workload with enough privilege to bypass its own
hook, for example by unsetting the audit library, using a static binary, or
`ptrace`.

- A report that a workload can exceed its own quota, without affecting another
  tenant, is not a new vulnerability by itself.
- A report that lets a workload reach another tenant's data, device, or
  namespace it was not granted is in scope; please report it through the
  process above.

## Response Process

Response times may be affected by weekends, holidays, or time-zone differences.
The maintainers aim to reply as soon as possible, ideally within 5 working days.

## Third-Party Dependencies

This project relies on third-party Go modules and container base images. We
monitor them with Dependabot and apply security patches promptly.

Thank you for helping keep the project secure.
