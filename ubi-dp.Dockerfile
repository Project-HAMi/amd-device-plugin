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
FROM registry.access.redhat.com/ubi9/ubi:9.8@sha256:16e9e43d99e181463513cc84a514859b1abb9460f1c4d9e208da4b4e51cd5cc3 AS builder
USER root
RUN dnf install -y 'dnf-command(config-manager)' && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/BaseOS/x86_64/os/ && \
    dnf config-manager --add-repo=https://mirror.stream.centos.org/9-stream/AppStream/x86_64/os/ && \
    rpm --import https://www.centos.org/keys/RPM-GPG-KEY-CentOS-Official && \
    dnf install git pkgconfig gcc gcc-c++ make glibc-devel binutils libdrm-devel hwloc-devel wget tar gzip -y && \
    dnf clean all
RUN wget https://golang.org/dl/go1.26.9.linux-amd64.tar.gz && \
    echo "42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d  go1.26.9.linux-amd64.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf go1.26.9.linux-amd64.tar.gz && \
    rm go1.26.9.linux-amd64.tar.gz
# AMD SMI SDK at the same ROCm release as the main image.
RUN printf '[rocm]\nname=ROCm 7.2.4\nbaseurl=https://repo.radeon.com/rocm/el9/7.2.4/main\nenabled=1\ngpgcheck=1\ngpgkey=https://repo.radeon.com/rocm/rocm.gpg.key\n' > /etc/yum.repos.d/rocm.repo && \
    dnf install -y amd-smi-lib && \
    dnf clean all
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


FROM registry.access.redhat.com/ubi9/ubi-init:9.8@sha256:2946d253611ad503048c0692434d98d8661cc2a4423ba9802a74635ef8b644b4
LABEL \
    name="amd-k8s-device-plugin" \
    maintainer="Project-HAMi maintainers" \
    vendor="Project-HAMi" \
    version="0.0.3" \
    release="0.0.3" \
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
RUN printf '[rocm]\nname=ROCm 7.2.4\nbaseurl=https://repo.radeon.com/rocm/el9/7.2.4/main\nenabled=1\ngpgcheck=1\ngpgkey=https://repo.radeon.com/rocm/rocm.gpg.key\n' > /etc/yum.repos.d/rocm.repo && \
    dnf install -y amd-smi-lib && \
    dnf clean all
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
