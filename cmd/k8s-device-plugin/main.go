/**
 * Copyright 2018 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

// Kubernetes (k8s) device plugin to enable registration of AMD GPU to a container cluster
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/allocator"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/hwloc"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/plugin"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
)

var gitDescribe string

type ResourceNamingStrategy string

const (
	StrategySingle ResourceNamingStrategy = "single"
	StrategyMixed  ResourceNamingStrategy = "mixed"
)

func ParseStrategy(s string) (ResourceNamingStrategy, error) {
	switch s {
	case string(StrategySingle):
		return StrategySingle, nil
	case string(StrategyMixed):
		return StrategyMixed, nil
	default:
		return "", fmt.Errorf("invalid resource naming strategy: %s", s)
	}
}

func getResourceList(resourceNamingStrategy ResourceNamingStrategy) ([]string, error) {
	var resources []string

	gpus := amdgpu.GetAMDGPUs()
	if len(gpus) == 0 {
		return resources, nil
	}
	partitionCountMap := amdgpu.UniquePartitionConfigCount(gpus)
	if amdgpu.IsHomogeneous(gpus) {
		// Homogeneous node will report only "gpu" resource if strategy is single. If strategy is mixed, it will report resources under the partition type name
		switch resourceNamingStrategy {
		case StrategySingle:
			resources = []string{"gpu"}
		case StrategyMixed:
			if len(partitionCountMap) == 0 {
				// If partitioning is not supported on the node, we should report resources under "gpu" regardless of the strategy
				resources = []string{"gpu"}
			} else {
				for partitionType, count := range partitionCountMap {
					if count > 0 {
						resources = append(resources, partitionType)
					}
				}
			}
		}
	} else {
		// Heterogeneous node reports resources based on partition types if strategy is mixed. Heterogeneous is not allowed if Strategy is single
		switch resourceNamingStrategy {
		case StrategySingle:
			return resources, fmt.Errorf("partitions of different styles across GPUs in a node are not supported with the single strategy, start the device plugin with the mixed strategy")
		case StrategyMixed:
			for partitionType, count := range partitionCountMap {
				if count > 0 {
					resources = append(resources, partitionType)
				}
			}
		}
	}
	return resources, nil
}

func main() {
	versions := [...]string{
		"AMD GPU device plugin for Kubernetes",
		fmt.Sprintf("%s version %s", os.Args[0], gitDescribe),
		hwloc.GetVersions(),
	}

	flag.Usage = func() {
		for _, v := range versions {
			fmt.Fprintf(os.Stderr, "%s\n", v)
		}
		fmt.Fprintln(os.Stderr, "Usage:")
		flag.PrintDefaults()
	}
	var pulse int
	var resourceNamingStrategy, allocatorPolicy, cdiSpecDir string
	var splitCount int
	var dmemBackend, muslFailClosed, reportNodeCapacity bool
	var ctrPath, containerdSocket string
	flag.IntVar(&pulse, "pulse", 0, "time between health check polling in seconds.  Set to 0 to disable.")
	flag.StringVar(&resourceNamingStrategy, "resource_naming_strategy", "single", "Resource strategy to be used: single or mixed")
	flag.IntVar(&splitCount, "split_count", 0, "How many workloads may share one GPU (HAMi device Count). 0 picks it per GPU: 2 on gfx12, whose throughput collapses above about 2 sharers, otherwise 10.")
	flag.BoolVar(&reportNodeCapacity, "report_node_capacity", false, "Publish the healthy GPUs' memory (amd.com/gpumem, MiB) and compute units (amd.com/gpucores) as node capacity and allocatable.")
	flag.StringVar(&cdiSpecDir, "cdi_spec_dir", "", "Write a CDI spec for amd.com/gpu here (for example /var/run/cdi) and inject devices through CDI. Empty uses device nodes.")
	flag.StringVar(&allocatorPolicy, "allocator_policy", "besteffort", "Preferred allocation policy: besteffort, binpack or spread")
	flag.BoolVar(&dmemBackend, "dmem_backend", true, "Also cap sliced allocations through the kernel dmem cgroup controller. Used only when the node has the dmem controller and the systemd cgroup driver, and the plugin sees the host cgroup hierarchy; see Project-HAMi/amd-hami-core#10.")
	flag.BoolVar(&muslFailClosed, "musl_fail_closed", false, "Refuse a sliced allocation whose image is not LD_AUDIT-compatible (musl, or statically linked), instead of silently running it unprotected. Requires the containerd socket and snapshotter data dir mounted with mountPropagation: HostToContainer (see Helm dp.muslFailClosed). Experimental; see Project-HAMi/amd-hami-core#3.")
	flag.StringVar(&ctrPath, "ctr_path", "/usr/local/bin/ctr", "Path to the ctr binary inside this pod, used only when -musl_fail_closed is set.")
	flag.StringVar(&containerdSocket, "containerd_socket", "/run/containerd/containerd.sock", "containerd socket path inside this pod, used only when -musl_fail_closed is set.")
	// this is also needed to enable glog usage in dpm
	flag.Parse()
	strategy, err := ParseStrategy(resourceNamingStrategy)
	if err != nil {
		glog.Errorf("%v", err)
		os.Exit(1)
	}

	if splitCount < 0 {
		glog.Errorf("split_count must not be negative, got %d", splitCount)
		os.Exit(1)
	}
	if _, err := allocator.NewPolicy(allocatorPolicy); err != nil {
		glog.Errorf("%v", err)
		os.Exit(1)
	}

	for _, v := range versions {
		glog.Infof("%s", v)
	}

	l := plugin.AMDGPULister{
		ResUpdateChan:      make(chan dpm.PluginNameList),
		Heartbeat:          make(chan bool),
		AllocatorPolicy:    allocatorPolicy,
		SplitCount:         splitCount,
		ReportNodeCapacity: reportNodeCapacity,
		CDISpecDir:         cdiSpecDir,
		DmemBackend:        dmemBackend,
		MuslFailClosed:     muslFailClosed,
		CtrPath:            ctrPath,
		ContainerdSocket:   containerdSocket,
	}
	manager := dpm.NewManager(&l)

	if pulse > 0 {
		go func() {
			glog.Infof("Heart beating every %d seconds", pulse)
			for {
				time.Sleep(time.Second * time.Duration(pulse))
				l.Heartbeat <- true
			}
		}()
	}

	go func() {
		// /sys/class/kfd only exists if ROCm kernel/driver is installed
		var path = "/sys/class/kfd"
		if _, err := os.Stat(path); err == nil {
			resources, err := getResourceList(strategy)
			if err != nil {
				glog.Errorf("Error occurred: %v", err)
				os.Exit(1)
			}
			if len(resources) > 0 {
				l.ResUpdateChan <- resources
			}
		}
	}()
	manager.Run()

}
