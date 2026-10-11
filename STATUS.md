# Status

What was checked, what was found and what is left, in plain words. Read [ONBOARDING.md](ONBOARDING.md) to learn how the project works.

## In one minute

- We re-checked **every merged pull request** of this repository (131) and of [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core) (19) on a real AMD GPU, and the AMD-related pull requests in [HAMi](https://github.com/Project-HAMi/HAMi) (20). This file and `ONBOARDING.md` cover all three; they live only here.
- Each one has one line below: the result and the one thing still not verified.
- We found **nine real bugs** and fixed them (table below). The partition work (#17) was tested again from the start: it also led to #169 (virtual functions stay CU-sliced in partition mode), #171 and #172.
- What is left: five open design questions, and the checks that need hardware we did not have.

## How we tested

- A real AMD Instinct MI325X (one GPU, shown to the machine as a virtual function) with k3s, the HAMi scheduler and this plugin built from `main`.
- First kernel 6.8, then kernel 7.0 (the kernel dmem memory cap needs 6.14 or newer).
- Tests that need several GPUs, a real compute-partition flip, RDNA cards or managed-memory spill could not run on this machine (see "What we could not check").

## Problems found and fixed

| Problem | Where it was found | Fix |
|---|---|---|
| A Pod that HAMi did not schedule failed at start with "no binding pod found". | #18 | #161 |
| A firmware test failed on kernel 7.0 because a debugfs line has a new field. | #38 | #162 |
| With `muslFailClosed`, the first Pod that used a new image failed, and every retry failed too. | #104 | #163 |
| Turning `reportNodeCapacity` off left old values on the node. | #157 | #164 |
| Images were built with Go 1.26.8, which has 11 known standard library problems. | build | #166 |
| `go.mod` used a grpc development tag instead of a release. | #136 | #167 |
| No issue or pull request templates in amd-hami-core; some `/kind` labels missing in both repos. | core #2, #15 | core #26 and new labels |
| With `-resource_naming_strategy=mixed` a virtual function was named `amd.com/spx_nps1` and `amd.com/gpu` went to zero. | #17 | #171 |
| The registration said 0 XCC per partition for DPX, QPX and CPX, because AMD SMI lists XCC for the current profile only. | #17 | #172 |
| On HAMi master a GPU registered with an empty type was never picked: the Pod stayed Pending with NodeInsufficientDevice. | HAMi #2815 | HAMi #3212 (open, tested, waits for a review) |
| Docs and README were behind the code. | all | #168 and core #27 |

## Open questions

- **Cores default.** A request without `amd.com/gpucores` gets all CUs of the GPU. A Pod that sets only `amd.com/gpumem` therefore blocks the whole GPU's compute, and only one runs at a time. Decide if memory-only Pods should share compute (HAMi pull request #3192 adds settings for this).
- **OWNERS.** Neither repo has an `OWNERS` file, so the approval bot reports "not approved". Decide who the approvers are.
- **Dependabot.** It can move a Go module to a development tag again.
- **k3s paths.** The `muslFailClosed` chart defaults are for RKE2. On k3s two values must be set (see the configuration docs).
- **Cordon.** `hami.io/device-cordon` works after the scheduler has seen the annotation. A Pod created within about a second can still be placed.

## Compute partitions: what is verified and what is not

Short answer: **the real partition flip (DPX, QPX, CPX) is not verified on any hardware yet.** It needs a bare metal machine with an Instinct GPU (MI300, MI325 or MI355).

- A cloud virtual machine with an SR-IOV virtual function (VF) cannot use it. The VF reports its partition (SPX) but cannot change it: the `available_compute_partition` file is missing and AMD SMI refuses a change (status 2). It also did not work on the Radeon RX 9060 XT used earlier.
- What was verified on the VF: the VF keeps working in both operating modes. It stays one CU-sliced GPU with the normal split count, a request to flip is skipped with a clear log line, and switching the mode while Pods run breaks nothing (no errors, no overlapping CUs). Fixed on the way: #169, #171 and #172.
- What is still open (checklist item F5 of the #17 re-test): flipping to DPX, QPX or CPX, registering the XCP partitions, and placing Pods on partitions of a real partitioned GPU. The code and its unit tests pass, and fixtures captured from an MI355X cover the registration, but nothing ran on a partitioned GPU.
- What is needed: one bare metal MI300, MI325 or MI355 node, then run the checks in section 11.0 of `ONBOARDING.md` with `OPERATING_MODE=partition` and `COMPUTE_PARTITION` set.

## What we could not check

- Several GPUs: XGMI peers with real links, spread and binpack order, mask order across GPUs, topology-aware scheduling, `amd.com/link-bind`.
- A real compute-partition flip. On a virtual function AMD SMI refuses it.
- RDNA cards: whole-WGP slices, the default of 2 sharers on gfx12, the RDNA4 family.
- A `hipMalloc` larger than the VRAM, served from host memory. It needs XNACK, which is off on the virtual function.
- A real hardware fault reported by the AMD Device Metrics Exporter. A stand-in with the same gRPC protocol was used.

## Remaining work

To be closed one by one. Each item says what is missing and what it needs.

### HAMi pull requests waiting for a review

- #3212 (empty registered type), #3213 (AMD guide and examples), #3190, #3192, #3187, #3191. After each merge, re-run its checks on HAMi master.

### Checks still open per HAMi pull request

| PR | Missing check | Needs |
|---|---|---|
| #1975 | Whether the AMD support claim in the roadmap matches what is built. | Reading, no hardware. |
| #2011 | Mutex with an idle and a used GPU side by side. | Two or more GPUs. |
| #2621 | Other policy orders and three or more policies. | This machine. |
| #3050 | Website links after the docs move. | Reading, no hardware. |
| #3174 | Scheduler core count against a real RDNA CU mask. | An RDNA card. |
| #3178 | Used bytes under a real allocation, power and temperature metrics. | This machine. |
| #3179 | Helm output of the percentage resource name. | This machine. |
| #3180 | ResourceQuota with init containers. | This machine. |
| #3182 | Excluding only the bad card when one of several fails. | Two or more GPUs. |
| #3186, #3187, #3191 | Real multi-NUMA and XGMI nodes (only a scheduler run with fake registration was done). | A multi-GPU Instinct node. |
| #3190 | Effect of the priority under HAMi slices. | This machine. |
| #3192 | A Pod created right after a cordon can still be placed. | A design decision. |
| #3193 | The two skipped e2e specs and the workflow run. | This machine and CI. |
| #3194 | Helm rendering and a missing RuntimeClass. | This machine. |

### Plugin and core checks that need other hardware

- Several GPUs with real XGMI links, mask order across GPUs, spread and binpack order.
- A real compute-partition flip (bare metal, not a virtual function).
- RDNA cards.
- XNACK, for a `hipMalloc` served from host memory.
- A real fault from the AMD Device Metrics Exporter.

### Pull requests of this repository, per reason

The "Still open" column of the table above now holds only what could not be run on this machine:

- Several GPUs, XGMI, NUMA across GPUs, policy order: #20, #59, #61, #85, #91, #92, #95, #96, #100, #102, #103, #108, #120, #153, #156.
- RDNA cards: #57, #97, #105, #107, #109, #151.
- Bare metal Instinct with real compute partitions: #17, #123, #145, #159, #160.
- Release publishing, arm64 and other Kubernetes versions: #7, #11, #12, #13, #48, #60, #128, #138.
- A real Device Metrics Exporter and a real hardware fault: #66, #127, #140, #152, #155.
- XNACK spill: #158.
- Large example images that were not pulled: #139, #141.
- Hosted services: Read the Docs (#50, #147) and the Security Insights validator (#46, #149).
- Dependabot can propose a development tag again (#136).

### Decisions

- Cores default, OWNERS files, Dependabot dev tags, k3s paths for `muslFailClosed`, cordon cache lag (see "Open questions").

## amd-hami-core pull requests

The memory library lives in [amd-hami-core](https://github.com/Project-HAMi/amd-hami-core). What the library was measured to do on the real GPU:

- In a 25 percent slice, a compute kernel ran at 215 kernels/s. Setting `HSA_CU_MASK` from inside the process did not widen it with the library (215), and did without it (1015).
- Two processes on one slice got 144.7 and 72.3 kernels/s with `AMD_TASK_PRIORITY` 0 and 3, against about 108.6 each with no priority.
- With a 1 GiB limit, `hipMalloc3D`, arrays, pools and `hipMemCreate` all returned out of memory, and `hipMemGetInfo` reported 1 GiB.
- Three processes each asking for 1.5 GiB under a 4 GiB limit: exactly two succeeded, in each of three rounds.
- The C tests pass under AddressSanitizer, UBSan and ThreadSanitizer, and the glibc 2.34 check passes.

| PR | Change | Result | Still open |
|---|---|---|---|
| #2 | docs: add AMD HAMi core contribution templates | Closed, not merged. Templates later added in #26. | Nothing |
| #7 | docs: clarify libamvgpu.so does memory limiting, not CU masking | README checked on main. A 25 percent slice ran at the same speed with and without the hook. | Nothing |
| #8 | fix: flag src/multiprocess/*.c NVIDIA leftovers as unbuilt, not wired in | Superseded. Files were removed in #20. Main builds without them. | Compute limiting on AMD was never ported or tested. |
| #9 | fix: restore glibc 2.34 ABI baseline, add a regression check | ABI check passes on the shipped library. Hook loaded on glibc 2.34 images. Musl does not load it. | Other build hosts and the musl case were not tested. |
| #11 | docs: add LICENSE and CONTRIBUTING.md | Both files exist on main. | Build commands in CONTRIBUTING.md were not run as written. |
| #12 | fix: restore_container_env was a no-op across the LD_AUDIT namespace boundary | In a sliced Pod the runtime sees the pod mask and device list, also in child processes. | A real vLLM or SGLang engine was not run. |
| #13 | fix: pin HSA_CU_MASK to the pod spec value | Escape test on a 25 percent slice: setenv escape was ignored with the hook, and gave the whole GPU without it. | Child processes without LD_AUDIT still escape the slice. |
| #14 | fix: make memory accounting exact under concurrency | All C tests pass under ASan, UBSan and TSan. Three processes under a 4096 MiB limit: two succeeded, one got out of memory. | Real frameworks and shared regions across many containers were not run. |
| #15 | docs: add security-insights.yml and SECURITY.md | Both files exist on main. | YAML was not checked against the schema and links were not tested. |
| #16 | ci: add CI, CodeQL and Scorecard workflows; add CODE_OF_CONDUCT.md | Workflow files and code of conduct exist. Last three runs of each workflow succeeded. | CI never runs on a GPU. |
| #17 | ci: pin ROCm base images by digest, add Dependabot config | Base images are pinned by digest and all actions use commit SHAs. Dependabot config exists. | Dependabot has not opened a bump PR yet. |
| #18 | fix: correct CMake build comment, use real glibc baseline check in Docker, sync security-insights.yml tools | Docker build of main succeeds. The glibc baseline check passes on the built library. | The log level fallback and fork behavior have no test. |
| #19 | chore: ignore all five test binaries, match CU mask example to README | .gitignore lists test binaries. README mask example matches the format the plugin sets. | Nothing |
| #20 | fix: publish the shared region with acquire/release, drop dead sources | test_init passes under ASan, UBSan and TSan. Dead sources are gone and the Docker build works. | Arm64 and the environ load path have no test. |
| #21 | fix: read the limit retry environment under the lock | test_retry passes under ASan, UBSan and TSan. | The case where the limit appears late in a child is not covered. |
| #22 | ci: keep the pinned ROCm images where Dependabot can update them | Pinned images live in .github/rocm files and ci.yml reads them. CI on main succeeds. | No Dependabot bump PR has been seen yet. |
| #23 | feat: set the HSA queue priority from AMD_TASK_PRIORITY | Two processes on one 25 percent slice: high priority 144.7 kernels/s, low 72.3. Without priority both about 108. | Queue wrapper and dlmopen lookup have no unit test. |
| #24 | feat: cap hipMalloc3D, arrays, pools and VMM, and report the limit as the total memory | With a 1024 MiB limit the reported total is 1024 MiB. Five allocator types return out of memory at 2 GiB. | Hard coded property offsets are untested across ROCm versions. |
| #25 | feat: serve hipMalloc from managed memory when HIP_OVERSUBSCRIBE is set | Unit test passes under ASan and UBSan. A 100 GB hipMalloc with memset worked. | Spill to host memory was not verified because XNACK is off. |

## HAMi pull requests

These are in the HAMi repository. "Not run on a GPU" means the change was read and its unit tests passed, but it was not exercised on the machine.

| PR | Change | Result | Still open |
|---|---|---|---|
| #1975 | docs: fix typo and mark AMD and MIG as supported in roadmap | Merged. Docs only. Not run on a GPU. | Whether the AMD support claim matches what is built. |
| #2011 | feat: new mutex GPU scheduling policy | Merged. Unit test and a real run: a Pod with `mutex` stays Pending (ExclusiveDeviceAllocateConflict) while another Pod holds the GPU, and a normal Pod can still share a GPU a mutex Pod holds. | Several GPUs (one idle, one used) were not available. |
| #2098 | chore: remove dead script, boilerplate file, and unused ParseConfig stubs | Merged. Only removals. Build and tests pass on the GPU host. | Nothing |
| #2621 | feat: support comma-separated gpu-scheduler-policy combinations | Merged. Real run: `binpack,mutex` and `mutex,spread` apply mutex (Pod refused), `spread,binpack` does not. | Other orders and three or more policies were not run. |
| #2815 | fix(amd): keep allocation annotations decodable for empty registered types | Merged. Unit tests and a real scheduler run with an empty registered type. That run found a bug: the Pod stayed Pending because the type match never succeeds for an empty type. Fixed in #3212 (open): the Pod is then scheduled and runs. | #3212 still needs a review before it can merge. |
| #3050 | chore: remove the docs now hosted on the website | Merged. Docs only, nothing to run. | Website links and migration were not checked. |
| #3093 | refactor(device): share the allocation check with every vendor plugin | Closed, not merged. Unit tests of pkg/device and the NVIDIA plugin package pass with -race on the host. | Nothing AMD specific, no GPU run. |
| #3174 | fix(amd): round core requests up to whole WGPs | Merged. Real scheduler: with cuPerWGP 1 a 1 percent request of 304 CUs books 3 CUs, with cuPerWGP 2 it books 4. | Scheduler count was not compared with a real RDNA CU mask. |
| #3178 | feat(monitor): report AMD GPU memory per container and host metrics | Closed, not merged. Unit tests pass with -race. Run on the real GPU: the monitor served the per-container limit (4 GiB for a 4096 MiB Pod), per-container used bytes and the host memory and utilization metrics. | Used bytes under a real allocation, power and temperature were not checked. |
| #3179 | feat(amd): support gpumem-percentage memory requests | Merged. Real Pods: 25 percent of the registered memory gave the right limit variable and allocation, a percentage-only Pod got `amd.com/gpu 1`, an explicit gpumem wins, and 101, -1 and 1.5 are rejected. | Helm output of the percentage name was not checked. |
| #3180 | feat(amd): check the namespace ResourceQuota in Fit | Merged. Real namespace with a ResourceQuota: Pods within the quota run, the next one is refused or stays Pending with ResourceQuotaNotFit, another namespace is not affected. | No test with init containers. |
| #3181 | feat(amd): turn amd.com/priority into AMD_TASK_PRIORITY | Closed, not merged (replaced by #3190). Unit tests pass. Run on the real GPU with the scheduler built from the branch: priority 0 and 3 became AMD_TASK_PRIORITY, the resource replaced a value set in the Pod env, and -1 was rejected by the webhook. | Effect of the priority under HAMi slices was not re-measured. |
| #3182 | fix(amd): judge node health by allocatable GPUs | Merged. Unit tests pass. Real node with allocatable GPUs at 0: a new Pod stays Pending and runs after recovery. | Partial failure exclusion of the bad card is untested. |
| #3186 | feat(amd): support amd.com/numa-bind for multi-GPU requests | Merged. Unit tests pass. Real Pod with `amd.com/numa-bind` schedules and runs on the one-GPU node. | Never run on a multi-GPU, multi-NUMA node. |
| #3187 | feat(amd): add amd.com/link-bind for XGMI-linked multi-GPU requests | Open, ready for review. Rebased on master. Run with the real scheduler on the MI325X with 3 extra GPUs and XGMI links written into the node registration: a linked pair was chosen, no links was refused, a 3-GPU clique was chosen and a chain was refused. | Real XGMI hardware and a real multi-GPU node. |
| #3188 | docs(amd): add the AMD GPU guide and examples | Closed, not merged. Re-opened as #3213 (open). All 8 example manifests were applied on the real GPU: the whole-GPU, cores, memory and memory percentage ones ran, the type and UUID ones ran or stayed Pending (CardTypeMismatch, CardUuidMismatch) as the comments say. | The guide text itself was only read. |
| #3190 | feat(amd): add amd.com/priority as AMD_TASK_PRIORITY | Open. Unit tests pass on the rebased code. On a real node the Pod got the variable and a bad value was rejected. | Effect of the priority under HAMi slices was not re-measured. |
| #3191 | feat(amd): make the topology-aware policy choose the most XGMI-linked GPUs | Open, ready for review (stacked on #3187), rebased on master. Same fake multi-GPU run: topology-aware chose the linked pair, the same-NUMA pair without links, and the linked NUMA-1 pair together with numa-bind. | Real XGMI hardware and a real multi-GPU node. |
| #3192 | feat(amd): default the GPU count, memory and cores, and honor device cordon | Open. Unit tests pass. On a real node defaults and cordon worked, four Pods ran with disjoint masks. Two review findings were fixed: a memory request without a limit is read, and an explicit 0 memory is kept as a whole GPU. | A Pod created right after the cordon can still be placed. |
| #3193 | test(e2e): add an AMD GPU end-to-end suite | Merged. On a real node 9 specs passed, 0 failed, 2 skipped (re-run on current master). | The two skipped specs and the workflow run were not checked. |
| #3194 | feat(amd): set a configurable runtimeClassName on AMD GPU Pods | Merged. Real Pods: a GPU Pod gets the class, an explicit class is kept, a Pod without a GPU is left alone. | Helm rendering and a missing RuntimeClass are untested. |

## Every pull request of this repository

| PR | Change | Result | Still open |
|---|---|---|---|
| #1 | Bump rocm-docs-core from 1.21.1 to 1.38.0 in docs/sphinx | Docs build with the new version finished fine. Only two old warnings, unrelated to the bump. | Nothing |
| #2 | Bump golang from 1.23.6-alpine3.21 to 1.25.5-alpine3.21 | Builder image builds, vet is clean, tests pass on a real GPU except one old debugfs test. A real Pod ran and memory limit worked. The labeller image built from this Dockerfile ran on the node and set the labels. | Nothing |
| #3 | Bump go-logr/logr from 1.4.3 to 1.4.4 | Vet and race tests pass on main. The running plugin logs normally. | Nothing |
| #4 | Bump ubi9/ubi-minimal from 9.4 to 9.8 | Both UBI images build on current main. The UBI labeller image ran on the node and set the labels. | Nothing |
| #5 | Bump alpine from 3.21.3 to 3.24.1 | Labeller image builds on main. Main now uses a newer Alpine than this PR. The labeller image ran on the node and set the labels. | Nothing |
| #6 | Bump actions/checkout from 6 to 7 | CI only change. Main pins checkout v7, so the bump landed. | Nothing |
| #7 | Bump google.golang.org/grpc from 1.65.0 to 1.84.0 | Plugin registers with kubelet over grpc, tests pass, and a real Pod got a GPU and memory limit. Restarting the kubelet with a GPU Pod running: allocatable was back in 6 seconds, the Pod kept running, a new Pod ran. | Other Kubernetes versions were not tried. |
| #8 | Bump google.golang.org/protobuf from 1.36.8 to 1.36.11 | Covered by working registration and passing tests. A real Pod ran with the memory limit enforced. | Nothing |
| #9 | Bump rocm/dev-ubuntu-22.04 from 7.0.2 to 7.2.4 | Closed, not merged. Main already has this ROCm version through other changes. | Nothing |
| #10 | Bump k8s.io/klog/v2 from 2.130.1 to 2.140.0 | Plugin logs look normal, tests pass, and a real Pod ran fine. | Nothing |
| #11 | Bump helm/chart-releaser-action from 1.6.0 to 1.7.0 | Workflow change only. Main pins the new version. | A real chart release with the new action was not run |
| #12 | Bump rocm/dev-ubuntu-24.04 from 7.0.2 to 7.2.4 | Image builds and runs on a real GPU, reading GPU state. A real Pod ran with the memory limit enforced. | Other GPU families and drivers were not tried |
| #13 | Bump controller-runtime from 0.19.0 to 0.25.1 | Labeller runs on the GPU node and sets labels. Vet and tests pass with a newer version. | Older Kubernetes server versions were not tried |
| #15 | docs: add AMD contribution templates | Template files are valid and labels exist. Four missing kind labels were added directly. GitHub lists the issue and pull request templates, the contributing guide and the code of conduct (community health 100 percent). | Nothing |
| #17 | feat(plugin): hardware compute partitions for Instinct GPUs | Tested again item by item on a virtual function: both operating modes, annotations over env, naming strategies, mode switches with Pods running (no errors, no overlap), all its tests. Two bugs found, fixed in #171 and #172. | A real partition flip and XCP registration need a bare metal MI300 or newer |
| #18 | fix admission error if another device plugin was installed | Helm install fix works. A bug in the no-annotation fallback was found, fixed in #161. The upstream ROCm plugin was run as a second DaemonSet on the node: kubelet refuses the second registration (device plugin already connected), the first plugin keeps serving, a GPU Pod ran with the hook, and removing the second plugin left everything working. | Nothing |
| #20 | Inject HIP_VISIBLE_DEVICES for Ray-compatible AMD allocations | Pods now get the variable. The Ray import error reproduced without it and went away with it. On one GPU the Pod gets HIP_VISIBLE_DEVICES=0 next to ROCR_VISIBLE_DEVICES. | The order for several GPUs such as 0,1 needs several GPUs. |
| #37 | Slice AMD GPUs by whole WGPs on RDNA | Closed, not merged. The same fix landed in #105 and #107. | Nothing |
| #38 | Skip amdgpu firmware test when debugfs is unavailable | The test was failing for another reason on a real GPU, an SMC field not parsed. Fixed in #162. On the host the test passes with debugfs mounted and skips without it. A card without debugfs used to skip the whole test on a multi-GPU node; it is now left out and the other cards are still compared. | The mixed case itself needs two GPUs. |
| #40 | Update docs Python dependencies to patched versions | Main has the patched versions and the docs build succeeds with them. pip-audit finds no known vulnerability in the docs pins. | Nothing |
| #42 | Bump go directive to 1.26.8 for standard-library fixes | Go version is set, builder image builds, vet is clean, and tests pass on a real GPU. govulncheck on the built plugin binary reports no vulnerability. | Nothing |
| #46 | Add SECURITY.md and SECURITY-INSIGHTS.yml | Both files exist on main and the issue config links to the private report form. | The insights file was not checked against its schema |
| #47 | Add OpenSSF Scorecard workflow | Workflow file is on main and its last three runs succeeded. The workflow ran after the protection change and its alerts are in the Security tab. | Nothing |
| #48 | Pin GitHub Actions to commit SHAs | Every action is pinned to a full commit hash and the workflows pass on main. | The image provenance step was not confirmed on a real push |
| #49 | Add CodeQL static analysis workflow | Workflow is on main and its recent runs succeeded. CodeQL has no open finding. | Nothing |
| #50 | Bump Read the Docs build image to ubuntu-24.04 | Setting is on main and the docs build with the same requirements. The docs build locally. | No Read the Docs project is published. |
| #56 | fix(allocator): allow besteffort init on single-GPU nodes | Allocator starts on a single-GPU node and a Pod requesting one GPU is allocated normally. The error branch no longer exists since #92 and #102; the peerless multi-device case is covered by TestBestEffortInitWithoutPeerLinks. | Nothing |
| #57 | fix(amdgpu): skip missing partition files on RDNA | Tests pass here, but this node has those files, so the RDNA path was not run. | RDNA log warnings were not checked on an RDNA GPU |
| #58 | fix(plugin): skip libamvgpu hook for whole-GPU requests | Whole-GPU Pod has no hook or memory limit. A sliced Pod has both. Both ran fine. Sliced Pods ran on alpine, ubuntu 18.04 (the loader refuses the hook, the Pod still starts), fedora and ubi9; a whole-GPU Pod on alpine has no hook. | Nothing |
| #59 | chore(allocator): fix staticcheck findings | Staticcheck shows only generated code findings and vet is clean. | Allocation results on a multi-GPU node were not compared |
| #60 | chore(deps): align k8s.io modules and use released protobuf | Modules are aligned on main, builder image builds, and tests pass. | Mixed Kubernetes versions were not tried on a real cluster |
| #61 | fix(labeller): compare render minor without narrowing int64 | Unit test passes. Labeller built from main runs on the MI325X node and sets the expected labels. | Several GPUs with different render minors were not tried. |
| #62 | Bump urllib3 from 2.2.2 to 2.8.0 in /docs/sphinx | Main pins this version or newer. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #63 | Bump gitpython from 3.1.43 to 3.1.62 in /docs/sphinx | Main pins this version or newer. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #64 | Bump pyjwt from 2.9.0 to 2.15.0 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #65 | Bump tornado from 6.4.2 to 6.5.9 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #66 | chore: replace deprecated io/ioutil and grpc.Dial | No old calls remain in the code. Plugin registers with kubelet and serves Allocate on the node. | Health exporter over the new gRPC client was not tested on a node with the exporter. |
| #67 | Bump k8s.io/kubelet from 0.31.0 to 0.37.1 | Closed, not merged. The same bump landed in #78, which also fixed the code changes this bump needed. | Nothing |
| #68 | Bump alpine from 3.24.1 to 3.24.2 | Main uses the new base. The labeller image builds and runs. | Nothing |
| #69 | Bump ubi9/ubi-init from 9.4 to 9.8 | Main uses the new base and the image builds. The UBI plugin image ran as the DaemonSet and registered the GPU. | Nothing |
| #70 | Bump soupsieve from 2.5 to 2.9 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #71 | Bump pygments from 2.18.0 to 2.20.0 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #72 | Bump cryptography from 43.0.1 to 50.0.0 in /docs/sphinx | Closed, not merged. It was replaced by #79, which bumps the same packages together. | Nothing |
| #73 | Bump requests from 2.32.3 to 2.33.0 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #74 | Bump idna from 3.7 to 3.15 in /docs/sphinx | Main pins a newer version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #75 | chore: fix remaining staticcheck findings in cgo code | Staticcheck reports nothing in the hand written code. go vet is clean. | Nothing |
| #76 | Bump pynacl from 1.5.0 to 1.6.2 in /docs/sphinx | Closed, not merged. It was replaced by #79, which bumps the same packages together. | Nothing |
| #77 | Bump jinja2 from 3.1.4 to 3.1.6 in /docs/sphinx | Main pins this version. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #78 | chore(deps): bump k8s.io/kubelet to v0.37.1 | Plugin builds and runs against the new kubelet API. It registers and serves Allocate on the node. Upgrade 0.0.2 to 0.0.3 to main and a rollback ran with a GPU Pod running; the Pod was not restarted. | Nothing |
| #79 | chore(docs): bump cryptography, pynacl, cffi and typing-extensions | Main pins these versions or newer. Docs build succeeds with the pins, with 2 unrelated warnings. | Nothing |
| #80 | chore(docker): align Go and ROCm versions across Dockerfiles | Dockerfiles use matching Go and ROCm 7.2.4 versions. All four images build on the node. | Nothing |
| #81 | chore: drop checked-in libamvgpu.so and refresh stale docs | The binary is gone and the image builds the hook from the submodule. Docs build succeeds. Pods load the hook. | Upgrade from a node that holds the old checked in hook was not tested. |
| #82 | docs: refresh README hook and delivery description | Docs build succeeds. A whole-GPU Pod gets no hook. A sliced Pod gets the hook and the limits, as the README says. Alpine and glibc 2.27 images ran a sliced Pod. | Nothing |
| #83 | docs: update CHANGELOG and development guide | Docs build succeeds and both files exist. The Go version in the guide was later corrected in #166. | Nothing |
| #84 | chore: remove unreferenced internal functions | go vet is clean. Tests pass with real GPU access. The deadcode tool reports nothing unreachable. | Nothing |
| #85 | feat(allocator): add spread and binpack policies | Each policy was set and a one GPU Pod ran every time. The flag reached the plugin. | The difference between spread and binpack needs a node with several GPUs. |
| #86 | chore: remove functions deadcode reports as unreachable | go vet is clean. Tests pass with real GPU access. The deadcode tool reports nothing unreachable. | Nothing |
| #87 | feat(plugin): per-GPU health from the DRM device | Hiding the GPU device made allocatable GPUs drop to 0 with an event. Restoring it brought them back. | A GPU that is present but unhealthy was not tested. |
| #88 | feat(plugin): publish compute queue slots in custominfo | The node annotation carries the compute queue count. Tests pass on the real GPU. | The scheduler using this value was not tested. |
| #89 | docs: document that CU slices are cooperative | Docs build succeeds. A process that sets the mask itself got the whole GPU. With the hook pinning it did not. | Nothing |
| #90 | feat(plugin): make the GPU split count configurable | Setting the count to 4 gave a count of 4 in the annotation and 4 allocatable GPUs. The default came back after reset. -1 is refused with a clear error, 3 and 0 register 3 and 10. | Nothing |
| #91 | feat(plugin): inject GPUs through CDI | With CDI on, the plugin wrote the spec file. A Pod got the kfd device and a card device. Turning it off restored device nodes. Turning CDI off leaves amd.com-gpu.json behind (documented). | Several GPUs were not tested. |
| #92 | fix(allocator): treat splits of one GPU as a single-GPU node | The single GPU with 4 slots was allocated normally under all three policies. | Nodes with several GPUs and no peer links were not tested here. |
| #93 | chore: fix staticcheck findings in cgo files | go vet is clean. Tests pass with real GPU access. Staticcheck reports nothing in hand written code. | Nothing |
| #94 | fix(docker): make the labeller and UBI images build again | All four images build on the node and the labeller runs. Tests pass. The UBI plugin and labeller images ran on the node. | Nothing |
| #95 | fix(plugin): recognize whole-GPU requests from the HAMi scheduler | A whole-GPU Pod through the scheduler got a full mask, no hook and no mount. Tests pass. Two sliced containers in one Pod got different masks and limits, and the lock was released. | A whole-GPU container next to a sliced one needs a second GPU. |
| #96 | fix(helm): expose the allocator policy | Setting binpack showed up in the DaemonSet args. It was restored to the default afterwards. | Policy results on a node with several GPUs were not observed. |
| #97 | fix(amdgpu): recognize the RDNA4 GPU family | Unit tests pass. This node is not RDNA4, so it reports its own family. | The RDNA4 label on a real RDNA4 card was not seen here. |
| #98 | docs: link the README by URL from the Sphinx docs | Docs build succeeds and the README links build without error. | Nothing |
| #99 | feat(plugin): cap sliced allocations with the kernel dmem cgroup, opt-in | A 4 GiB slice got the matching cap on its cgroup. A 2 GiB allocation worked and 8 GiB failed, even without the hook. | Allocations through hipMallocAsync pools and several GPUs were not tested. |
| #100 | test(plugin): lock HSA_CU_MASK order to ROCR_VISIBLE_DEVICES on multi-GPU | The mask order test passes. One GPU gave the expected mask and device list. | The order across several GPUs needs a multi GPU node. |
| #101 | fix(plugin): allocate GPUs without a ROCr UUID by container-local index | Unit tests pass. This GPU has a ROCr UUID, so the by index path did not run on hardware. | A GPU without a ROCr UUID, such as an APU, was not tried. |
| #102 | fix(allocator): weight GPU pairs without a direct link through the host | Allocator tests pass. This needs several GPUs, so it was not seen on the node. | A node with two GPUs and no direct links was not tested. |
| #103 | fix(plugin): set the memory limit per GPU on multi-GPU slices | A sliced Pod got the per GPU limit variable for index 0. The two GPU case is covered by a unit test. | A Pod with two GPUs was not run on hardware. |
| #104 | feat(plugin): fail closed on LD_AUDIT-incompatible images, opt-in | An Alpine sliced Pod was rejected. A glibc Pod got the hook. A whole-GPU Pod on Alpine started. Bug found, fixed in #163. | Nothing |
| #105 | fix(cuallocation): allocate whole WGPs on RDNA | Allocation tests pass. This GPU is CDNA and reports one CU per group, so the RDNA path was not seen. | Real RDNA behavior was not seen on this node. |
| #106 | fix(plugin): let the dmem cgroup back the musl fail-closed check | On the new kernel an Alpine sliced Pod, refused before, now starts without the hook. The cgroup cap backs the limit. | Several GPUs and hipMallocAsync use on musl images were not tested. |
| #107 | feat(plugin): publish cuPerWGP so the scheduler accounts whole WGPs | The annotation carries the per group CU value, 1 on this GPU. Tests pass. | Scheduler rounding needs an RDNA node. |
| #108 | test(plugin): cover the per-GPU dmem cap | Tests pass on the node. A real Pod cgroup got the right cap for a 4 GiB slice. | Caps for several GPUs need a node with more than one GPU. |
| #109 | feat(plugin): default to 2 sharers per GPU on gfx12 | This GPU still gets 10 sharers by default. The gfx12 value of 2 is covered only by a unit test. | A real gfx12 card was not available to confirm the value of 2. |
| #110 | feat(plugin): cap sliced VRAM with dmem by default where supported | With the default settings a sliced Pod got the cap on the new kernel. On the old kernel the plugin fell back and logged it. | Nodes using cgroupfs or cgroup v1 were not tested. |
| #111 | chore: bump amd-hami-core to pin HSA_CU_MASK | Checked on main with a 25 percent slice. A process that sets the mask itself stays slow with the hook (215 kernels/s) and runs fast without it (1015). | Other GPU families and core percentages are not tested. |
| #112 | chore(helm): drop the bundled logo.png | Docs build and chart lint pass. The logo file is gone from the chart. The icon URL loads (HTTP 200, image/png). | Nothing |
| #113 | fix(deps): bump grpc past GO-2026-6443 | Vulnerability scan is clean on main. This development version was later replaced by the released one in #167. | Nothing |
| #114 | chore(security): address OpenSSF Scorecard findings | Scorecard runs pass on the last three runs. Actions are pinned to commit hashes and checkouts keep no credentials. The fuzz test runs inside go test in CI and the image builds pass. | Nothing |
| #115 | fix(ci): skip existing chart releases | Workflow has the skip setting. Chart releases 0.0.1, 0.0.2 and 0.0.3 exist and 0.0.3 installs. | Nothing |
| #116 | docs: rewrite the README and user guide for the HAMi plugin | Docs build passes. Install, allocatable count, node annotation, memory cap and musl behavior were checked on a real node. | The new slice example pod was not applied to a cluster. |
| #117 | chore: bring repository files up to date | Docs build passes. Repository files are present and match the chart. Chart lint is clean with every option on. The changelog now lists the 21 missing pull requests (#180). | Nothing |
| #118 | fix(utils): release node locks reliably and keep device decode aligned | Eight sliced Pods started at once. All finished, ranges never overlapped, and no node lock was left behind. A Pod whose first container has no GPU ran; the node lock was released. | Nothing |
| #119 | fix: harden node labeller, chart RBAC and image CI | Vet and race tests pass on a real GPU node. The labeller runs as a DaemonSet and sets labels. Chart lints cleanly. Stale labels are removed when the labeller starts. | They are not removed on a live label edit, only at the next start. |
| #120 | fix(allocator): handle split devices in besteffort and spread | Split devices (4 per GPU) pass through besteffort, spread and binpack. Pods were allocated. | Real multi-GPU nodes with must_include requests were not run. |
| #121 | fix(libcheck,cuallocation): pick the loader the image uses, keep bitmaps intact on error | The right loader is chosen for alpine (refused), ubuntu and debian (hook on). Eight slices ended with separate CU ranges. Sliced Pods ran on fedora and ubi9. | Nothing |
| #122 | docs: correct hook, dmem, labeller and chart details | Docs build passes. Hook, memory cap, labels and chart values match real behavior. The chart renders on 1.19 and is refused on 1.18. | Nothing |
| #123 | fix(amdgpu): reset per-GPU discovery state and map partitions to parent health | Unit tests pass. With one GPU, health turned false when the exporter said unhealthy and true when healthy. | Partitioned GPUs on a real Instinct node were not tested. |
| #124 | chore: clear remaining lint nits outside the plugin package | Vet is clean and race tests pass. The only lint error came from missing AMD SMI headers in the test container. | Nothing |
| #125 | fix(plugin): guard discovery maps, free memory-only slices, end stale streams | Tests pass. Restarts run without errors. A memory-only request gets all CUs from the scheduler, so that path is not reached. | The memory-only slice path and a live kubelet run are not verified. |
| #126 | chore: bump amd-hami-core for exact memory accounting | Submodule has the fix. Three parallel 1.5 GiB allocations under a 4096 MiB limit gave exactly two successes in each of three rounds. CI builds the image with the new library commit. | Nothing |
| #127 | fix(exporter): log exporter outages once, not every health check | With the exporter gone, one warning appeared over about 10 health checks. The GPU count stayed at 10. | Only a test stand-in exporter was used, not the real one. |
| #128 | chore: tidy root files and record recent changes | Docs build passes. Root files exist and the changelog lists 0.0.2 and 0.0.3. There is no OWNERS file. | UBI image build on arm64 is not tested. |
| #129 | fix(labeller): require the node name, share topology lookup, use kernel family ids | Labeller starts with a node name and labels the node. Without a cluster it fails at the kube config. Family label shows AI. | Older setups that gave the node name another way are not tested. |
| #130 | fix: guard device requests, read image libc inside the image, scan once | Tests pass. Alpine, ubuntu and debian images are handled. A gap for images not yet pulled was fixed in #163. | A failed first registration and a retry have no test. |
| #131 | fix(helm): label the DaemonSet, make log verbosity a value, default the tag | Template renders verbosity and labels. The default tag follows the chart version. Chart lints cleanly. | Nothing. The default tag pulls the published image, which only works with the chart of the same release (documented in the installation guide). |
| #132 | docs: note the image tag for main, document log verbosity, fix a typo | Docs build passes. Verbosity and image tag text match the chart. The note that chart and image must match was missing and is now in the installation guide. | Nothing |
| #133 | chore: bump amd-hami-core for security/CI hardening and pthread_once fix | Submodule has the fix. Init test passes under sanitizers and the hook loads in real Pods. | Instinct cards and partition modes are not covered. |
| #134 | ci: vet and race-test, render optional chart branches, drop checkout tokens | CI runs vet and race tests, renders chart options and keeps no checkout tokens. CI is green and the same commands pass on a GPU node. | CI has no GPU, so hardware paths are not run there. |
| #135 | chore(deps): bump ubi9/ubi-init digest | Base image is pinned by digest and the UBI plugin image builds. The built UBI image ran on the node. | Nothing |
| #136 | chore(deps): bump grpc to 1.86.0-dev | Bug found: a development tag was pulled in. Fixed in #167 with the release. Scan is clean and a hooked Pod runs. | Dependabot may propose a development tag again. |
| #137 | chore(deps): bump golang to 1.27-alpine | Labeller image builds with the new Go version and labels the node. | Nothing |
| #138 | chore(deps): bump actions/attest-build-provenance to 4.2.2 | Workflow uses the new version pinned by hash. CI is green on main. | The attestation itself is made only on a publish build and was not inspected. |
| #139 | fix(examples): make the JAX pods run, stop the TensorFlow pod looping | All example manifests pass a server side dry run. The Pods were not run because the images are large. | No example Pod was run on a GPU. |
| #140 | fix(exporter): count only the query result as the exporter coming back | After the exporter returned, one "reachable again" message appeared after the next good query, not when the stale file existed. | The real exporter was not used, only a test stand-in. |
| #141 | docs(examples): note the GPU size the vLLM example needs | Docs build passes. The note is text only. | The vLLM example was never run on a 16 GB or larger card. |
| #142 | chore: release v0.0.2 | GitHub has release v0.0.2 and chart release 0.0.2. Version 0.0.3 replaces it. The published 0.0.2 image ran with its chart and registered the GPU. | Nothing |
| #143 | chore: bump amd-hami-core for the init guard race fix | Submodule has the fix. Init test passes under the thread checker. | The fix was not shown to change behavior on a real node. |
| #144 | chore: release v0.0.3 | Release is marked Latest. The published chart installs, registers 10 GPUs and a sliced Pod gets the right settings. It lacks later fixes (#161 to #164). Upgrade from 0.0.2 and a rollback ran. | Nothing |
| #145 | test: make GPU tests assert, drop an unused lister field | GPU tests now assert and pass on a real GPU instead of skipping. Vet and race tests pass. | Partitioned Instinct GPUs were not tested. |
| #146 | chore: bump amd-hami-core for the limit retry race fix | Submodule has the fix. Retry test passes under memory and thread checkers. A Pod without gpumem gets the whole memory as its limit (261824m). | Nothing |
| #147 | chore: update docs dependencies, build docs on Python 3.12 | Docs build passes with the new dependencies. The docs build on Python 3.12 without errors. | No Read the Docs project is published yet. |
| #148 | ci: read the CodeQL ROCm image from the Dockerfile | Workflow reads the image from the Dockerfile and ran successfully on main. Digest pins and the digest-less line both give the image name. | Nothing |
| #149 | chore: point security insights at the raw file, ignore secret files | Docs build passes. The file points at the raw link and secret files are ignored. The raw URL answers 200. | The file was not run through the Security Insights validator. |
| #150 | feat(monitor): report per-container VRAM and host GPU metrics | Host metrics are served. Per-container used and limit bytes were read on kernel 7.0. | Nothing |
| #151 | docs: say the gfx12 sharing loss depends on the workload | Docs build passes. Wording matches the code (10 sharers by default, 2 on gfx12). | The loss on RDNA4 cards cannot be measured on this GPU. |
| #152 | feat: mark GPUs the exporter reports unhealthy in the node annotation | With a test exporter, unhealthy moved the count from 10 to 0, set health false and sent a node event. | The real exporter was not used. |
| #153 | feat: publish the XGMI peers of each GPU in the node annotation | Single GPU node correctly shows no peers. Unit tests pass, including the partition fix. | Real multi-GPU XGMI links need an 8 GPU node and were not run. |
| #154 | feat(monitor): report the host GPU memory capacity | Total memory metric equals the value in sysfs. Used memory, utilization and collect success are served too. | Cards without the memory file and iGPUs were not checked. |
| #155 | feat: record node events when a GPU turns unhealthy, leaves or recovers | Hiding the GPU sent a missing event and restoring it sent a recovered event. | The unhealthy event was not triggered. |
| #156 | feat(plugin): add -exclude_gpus and -hip_log_level options | Log level reached a sliced Pod. Excluding a GPU cleared it and removing the setting brought it back. Excluding the only GPU by PCI address registers nothing, and clearing the list brings it back. | One GPU of several was not excluded. |
| #157 | feat(plugin): report healthy GPU memory and compute units as node capacity | Capacity showed up when on. Bug found: it stayed after switching off. Fixed in #164. | Nothing |
| #158 | feat(plugin): add -device_memory_scaling for memory oversubscription | Scaling 1.5 registered 1.5 times the memory and Pods got the oversubscribe setting. Resetting restored it. | A real allocation larger than VRAM spilling to host memory is not verified. |
| #159 | fix(plugin): keep the device ID stable across operating modes | The registered GPU ID is the same in cu mode and partition mode. | Real partitionable Instinct GPUs and XCP partition IDs were not tested. |
| #160 | feat(plugin): warn when the operating mode changes under running Pods | With a Pod running, changing the mode and restarting logged a warning and sent a node event naming the Pod. | A GPU with real hardware partitions was not tested. |
