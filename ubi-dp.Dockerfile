# Copyright 2024 Advanced Micro Devices, Inc.  All rights reserved.
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
# AMD SMI 27 (ROCm 10.0), the same library as the main image; see Dockerfile.
FROM ghcr.io/project-hami/amd-dra-driver:0.4.0@sha256:1f3a182b11203c59e49833c49b9c81ec1756846091bbfce5fcdb781d7c1ae05c AS amdsmi-lib

FROM registry.access.redhat.com/ubi9/ubi:9.8@sha256:48393e4261b2544f652c915b3e052256ddf66ee95cd04dc0362191f302c4e94a AS builder
USER root
RUN dnf install -y 'dnf-command(config-manager)' && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/BaseOS/x86_64/os/ && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/AppStream/x86_64/os/ && \
    rpm --import https://www.centos.org/keys/RPM-GPG-KEY-CentOS-Official && \
    dnf install git pkgconfig gcc gcc-c++ make glibc-devel binutils libdrm-devel hwloc-devel wget tar gzip -y && \
    dnf clean all
RUN wget https://golang.org/dl/go1.27.2.linux-amd64.tar.gz && \
    echo "ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5  go1.27.2.linux-amd64.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf go1.27.2.linux-amd64.tar.gz && \
    rm go1.27.2.linux-amd64.tar.gz
# The AMD SMI header from ROCm 7.2.4, linked against the AMD SMI 27 library.
RUN printf '[rocm]\nname=ROCm 7.2.4\nbaseurl=https://repo.radeon.com/rocm/el9/7.2.4/main\nenabled=1\ngpgcheck=1\ngpgkey=https://repo.radeon.com/rocm/rocm.gpg.key\n' > /etc/yum.repos.d/rocm.repo && \
    dnf install -y amd-smi-lib && \
    dnf clean all
COPY --from=amdsmi-lib /usr/lib64/libamd_smi.so.27.0.0 /usr/lib64/librocm_sysdeps_*.so* /opt/rocm/lib/
RUN ln -sf libamd_smi.so.27.0.0 /opt/rocm/lib/libamd_smi.so
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
RUN mkdir -p /go/src/github.com/Project-HAMi/amd-device-plugin
ADD . /go/src/github.com/Project-HAMi/amd-device-plugin
WORKDIR /go/src/github.com/Project-HAMi/amd-device-plugin/cmd/k8s-device-plugin
RUN go install \
    -ldflags="-X main.gitDescribe=$(git -C /go/src/github.com/Project-HAMi/amd-device-plugin/ describe --always --long --dirty)"

FROM rocm/dev-ubuntu-22.04:7.2.4@sha256:a50a8547101ac9e7c35848ff609a56335214b789b468c9eb05dcd006a4a38c25 AS hami-core
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    cmake build-essential \
    && rm -rf /var/lib/apt/lists/*
COPY amd-hami-core/ /build/amd-hami-core/
RUN cd /build/amd-hami-core && make -f Makefile.hip clean all


FROM registry.access.redhat.com/ubi9/ubi-init:9.8@sha256:d58c91058c10ad02ac18c5048e8c6ce279074d5691e9f9b08177e0e6b0efbe1a
LABEL \
    name="amd-k8s-device-plugin" \
    maintainer="Project-HAMi maintainers" \
    vendor="Project-HAMi" \
    version="0.0.4" \
    release="0.0.4" \
    summary="The AMD K8s Device Plugin enables the registration of AMD GPUs in your Kubernetes cluster for compute workloads." \
    description="The AMD K8s Device Plugin enables the registration of AMD GPUs in your Kubernetes cluster for compute workloads. With the appropriate hardware and this plugin deployed in your Kubernetes cluster, you will be able to run jobs that require AMD GPU."
RUN mkdir -p /licenses && \
    dnf install -y ca-certificates libdrm && \
    rpm --import https://www.centos.org/keys/RPM-GPG-KEY-CentOS-Official && \
    dnf install -y 'dnf-command(config-manager)' && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/BaseOS/x86_64/os/ && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/AppStream/x86_64/os/ && \
    dnf install -y hwloc && \
    dnf clean all
COPY --from=amdsmi-lib /usr/lib64/libamd_smi.so.27.0.0 /usr/lib64/librocm_sysdeps_*.so* /opt/rocm/lib/
RUN ln -s libamd_smi.so.27.0.0 /opt/rocm/lib/libamd_smi.so.27
ENV LD_LIBRARY_PATH=/opt/rocm/lib
ADD ./LICENSE /licenses/LICENSE
RUN mkdir -p /opt/hami/bin /opt/hami/lib/amd
WORKDIR /root/
COPY --from=builder /go/bin/k8s-device-plugin .
COPY --from=hami-core /build/amd-hami-core/build-hip/libamvgpu.so /opt/hami/lib/amd/libamvgpu.so
COPY --from=builder /go/src/github.com/Project-HAMi/amd-device-plugin/scripts/amd-vgpu-init.sh /opt/hami/bin/amd-vgpu-init.sh
RUN chmod 0555 /opt/hami/bin/amd-vgpu-init.sh \
    && test -s /opt/hami/lib/amd/libamvgpu.so
CMD ["./k8s-device-plugin", "-logtostderr=true", "-stderrthreshold=INFO", "-v=5"]
