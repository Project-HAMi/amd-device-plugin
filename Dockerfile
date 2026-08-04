# Copyright 2022 Advanced Micro Devices, Inc.  All rights reserved.
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.
FROM rocm/dev-ubuntu-24.04:7.0.2 AS rocm-runtime
FROM rocm/dev-ubuntu-22.04:7.2.4 AS amdsmi-sdk
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    cmake build-essential \
    && rm -rf /var/lib/apt/lists/*
COPY amd-hami-core/ /build/amd-hami-core/
RUN cd /build/amd-hami-core && make -f Makefile.hip clean all

FROM docker.io/golang:1.25 AS builder
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git pkg-config build-essential libdrm-dev libhwloc-dev \
    && rm -rf /var/lib/apt/lists/*
# Keep the AMD SMI API at ROCm 7.0.2, but take it from Ubuntu 22.04 so the
# library has an older glibc/libstdc++ baseline than the Ubuntu 24.04 runtime.
COPY --from=amdsmi-sdk /opt/rocm/include/amd_smi /opt/rocm/include/amd_smi
COPY --from=amdsmi-sdk /opt/rocm-7.0.2/share/amd_smi/amdsmi/libamd_smi.so /opt/rocm/lib/libamd_smi.so
COPY --from=amdsmi-sdk /usr/lib/x86_64-linux-gnu/libstdc++.so.6 /opt/rocm/lib/libstdc++.so.6
RUN ln -s libstdc++.so.6 /opt/rocm/lib/libstdc++.so
RUN mkdir -p /go/src/github.com/Project-HAMi/amd-device-plugin
ADD . /go/src/github.com/Project-HAMi/amd-device-plugin
WORKDIR /go/src/github.com/Project-HAMi/amd-device-plugin/cmd/k8s-device-plugin
RUN go install \
    -ldflags="-X main.gitDescribe=$(git -C /go/src/github.com/Project-HAMi/amd-device-plugin/ describe --always --long --dirty 2>/dev/null || echo unknown)"

FROM rocm-runtime
LABEL \
    org.opencontainers.image.source="https://github.com/Project-HAMi/amd-device-plugin" \
    org.opencontainers.image.authors="Project-HAMi maintainers" \
    org.opencontainers.image.vendor="Project-HAMi" \
    org.opencontainers.image.licenses="Apache-2.0"
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libdrm2 libhwloc15 && rm -rf /var/lib/apt/lists/*
# The executable records the libamd_smi.so.26 SONAME. Override the runtime
# target so it uses the Ubuntu 22.04-built, but still ROCm 7.0.2, library.
COPY --from=amdsmi-sdk /opt/rocm-7.0.2/share/amd_smi/amdsmi/libamd_smi.so /opt/rocm-7.0.2/lib/libamd_smi.so.26.0.70002
RUN mkdir -p /opt/hami/bin /opt/hami/lib/amd
WORKDIR /root/
COPY --from=builder /go/bin/k8s-device-plugin .
COPY --from=amdsmi-sdk /build/amd-hami-core/build-hip/libamvgpu.so /opt/hami/lib/amd/libamvgpu.so
COPY --from=builder /go/src/github.com/Project-HAMi/amd-device-plugin/scripts/amd-vgpu-init.sh /opt/hami/bin/amd-vgpu-init.sh
RUN chmod 0555 /opt/hami/bin/amd-vgpu-init.sh \
    && test -s /opt/hami/lib/amd/libamvgpu.so
CMD ["./k8s-device-plugin", "-logtostderr=true", "-stderrthreshold=INFO", "-v=5"]
