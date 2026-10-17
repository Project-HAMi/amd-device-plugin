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
FROM rocm/dev-ubuntu-24.04:7.2.4@sha256:bdc8e61026cbb844ede93d44d2c50055f51ebb2041906b60182bf3bee3139054 AS rocm-runtime
# AMD SMI 27 (ROCm 10.0) as vendored by the DRA driver. ROCm 10.0 has no
# package repository; older AMD SMI releases report another UUID and product
# name for some cards than the host tools and the DRA driver do.
FROM ghcr.io/project-hami/amd-dra-driver:0.3.0@sha256:f221d3c844c7264d10d003eb1b835a8f3d92bbe5df5090182101297b28f120b6 AS amdsmi-lib
FROM rocm/dev-ubuntu-22.04:7.2.4@sha256:a50a8547101ac9e7c35848ff609a56335214b789b468c9eb05dcd006a4a38c25 AS amdsmi-sdk
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    cmake build-essential \
    && rm -rf /var/lib/apt/lists/*
COPY amd-hami-core/ /build/amd-hami-core/
RUN cd /build/amd-hami-core && make -f Makefile.hip clean all

FROM docker.io/golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    git pkg-config build-essential libdrm-dev libhwloc-dev \
    && rm -rf /var/lib/apt/lists/*
# The 7.2.4 header declares the same types and calls as AMD SMI 27 for what
# the plugin uses; the library itself is AMD SMI 27.
COPY --from=amdsmi-sdk /opt/rocm/include/amd_smi /opt/rocm/include/amd_smi
COPY --from=amdsmi-lib /usr/lib64/libamd_smi.so.27.0.0 /usr/lib64/librocm_sysdeps_*.so* /opt/rocm/lib/
RUN ln -s libamd_smi.so.27.0.0 /opt/rocm/lib/libamd_smi.so
COPY --from=amdsmi-sdk /usr/lib/x86_64-linux-gnu/libstdc++.so.6 /opt/rocm/lib/libstdc++.so.6
RUN ln -s libstdc++.so.6 /opt/rocm/lib/libstdc++.so
RUN mkdir -p /go/src/github.com/Project-HAMi/amd-device-plugin
ADD . /go/src/github.com/Project-HAMi/amd-device-plugin
WORKDIR /go/src/github.com/Project-HAMi/amd-device-plugin/cmd/k8s-device-plugin
RUN go install \
    -ldflags="-X main.gitDescribe=$(git -C /go/src/github.com/Project-HAMi/amd-device-plugin/ describe --always --long --dirty 2>/dev/null || echo unknown)"
RUN CGO_ENABLED=0 go install ../k8s-vgpu-monitor

FROM rocm-runtime
LABEL \
    org.opencontainers.image.source="https://github.com/Project-HAMi/amd-device-plugin" \
    org.opencontainers.image.authors="Project-HAMi maintainers" \
    org.opencontainers.image.vendor="Project-HAMi" \
    org.opencontainers.image.licenses="Apache-2.0"
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libdrm2 libhwloc15 && rm -rf /var/lib/apt/lists/*
# The executable records the libamd_smi.so.27 SONAME. The library finds its
# rocm_sysdeps libraries next to itself ($ORIGIN).
COPY --from=amdsmi-lib /usr/lib64/libamd_smi.so.27.0.0 /usr/lib64/librocm_sysdeps_*.so* /opt/hami/lib/amdsmi/
RUN echo /opt/hami/lib/amdsmi > /etc/ld.so.conf.d/hami-amdsmi.conf && ldconfig
RUN mkdir -p /opt/hami/bin /opt/hami/lib/amd
WORKDIR /root/
COPY --from=builder /go/bin/k8s-device-plugin .
COPY --from=builder /go/bin/k8s-vgpu-monitor .
COPY --from=amdsmi-sdk /build/amd-hami-core/build-hip/libamvgpu.so /opt/hami/lib/amd/libamvgpu.so
COPY --from=builder /go/src/github.com/Project-HAMi/amd-device-plugin/scripts/amd-vgpu-init.sh /opt/hami/bin/amd-vgpu-init.sh
RUN chmod 0555 /opt/hami/bin/amd-vgpu-init.sh \
    && test -s /opt/hami/lib/amd/libamvgpu.so
CMD ["./k8s-device-plugin", "-logtostderr=true", "-stderrthreshold=INFO", "-v=5"]
