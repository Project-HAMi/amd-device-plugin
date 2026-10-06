/**
# Copyright (c) Advanced Micro Devices, Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the \"License\");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an \"AS IS\" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

// Package exporter is a collection of utility to access health exporter grpc service
// hosted by amd-metrics-exporter service
package exporter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/exporter/metricssvc"
	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// healthSocket is the exporter's gRPC socket; tests point it elsewhere.
var healthSocket = "/var/lib/amd-metrics-exporter/amdgpu_device_metrics_exporter_grpc.socket"

const (
	queryTimeout = 5 * time.Second
	xcpPrefix    = "amdgpu_xcp_"
)

// getGPUHealth returns device id map with health state if the metrics service
// is available else returns error
func getGPUHealth() (hMap map[string]string, err error) {
	// if the exporter service is not available, do not proceed
	healthSvcAddress := fmt.Sprintf("unix://%v", healthSocket)
	if _, err = os.Stat(healthSocket); err != nil {
		return
	}

	hMap = make(map[string]string)

	// the connection is short lived as the exporter can come and go
	// independently
	conn, err := grpc.NewClient(healthSvcAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	// NewClient does not dial, so its success says nothing about the
	// exporter; only the List result below marks it reachable again
	if err != nil {
		logExporterState(err)
		return
	}

	defer func() { _ = conn.Close() }()
	client := metricssvc.NewMetricsServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	resp, err := client.List(ctx, &emptypb.Empty{})
	if logExporterState(err) {
		return
	}
	for _, gpu := range resp.GPUState {
		if gpu.Health == strings.ToLower(pluginapi.Healthy) {
			hMap[gpu.Device] = pluginapi.Healthy
		} else {
			hMap[gpu.Device] = pluginapi.Unhealthy
		}
	}
	return
}

// exporterDown remembers that the exporter was unreachable, so a stale
// socket left by a stopped exporter logs once instead of on every health
// check.
var exporterDown atomic.Bool

// warnf is glog.Warningf, replaceable in tests.
var warnf = glog.Warningf

// logExporterState logs when the exporter becomes unreachable or reachable
// again, and reports whether err is set.
func logExporterState(err error) bool {
	if err != nil {
		if !exporterDown.Swap(true) {
			warnf("metrics exporter unreachable, using DRM health only until it returns: %v", err)
		} else {
			glog.V(4).Infof("metrics exporter still unreachable: %v", err)
		}
		return true
	}
	if exporterDown.Swap(false) {
		glog.Info("metrics exporter reachable again")
	}
	return false
}

// PopulatePerGPUDHealth populate the per gpu health status if available,
// else return simple health status
func PopulatePerGPUDHealth(devs []*pluginapi.Device, defaultHealth string) {
	// on error the map is nil or empty, so every device gets defaultHealth
	hMap, _ := getGPUHealth()
	applyHealth(devs, hMap, defaultHealth, partitionBDF("/sys/devices/platform", "/sys/class/kfd/kfd"))
}

// applyHealth sets each device to the exporter health of its GPU. The exporter
// keys GPUs by PCI BDF while device ids are "<bdf or partition>#<slot>" splits;
// gpuBDF maps the part before '#' to the BDF.
func applyHealth(devs []*pluginapi.Device, hMap map[string]string, defaultHealth string, gpuBDF func(string) string) {
	for _, d := range devs {
		d.Health = defaultHealth
		if h, ok := hMap[gpuBDF(strings.SplitN(d.ID, "#", 2)[0])]; ok {
			d.Health = h
		}
	}
}

// partitionBDF maps an MI300 XCP partition (amdgpu_xcp_N) to its GPU's PCI
// BDF through the partition's render node and the KFD topology. Other ids
// are returned unchanged.
func partitionBDF(platformDir, topoRoot string) func(string) string {
	var renderBDF map[int]string
	return func(id string) string {
		if !strings.HasPrefix(id, xcpPrefix) {
			return id
		}
		if renderBDF == nil {
			renderBDF = amdgpu.GetDevIdsFromTopology(topoRoot)
		}
		renders, _ := filepath.Glob(filepath.Join(platformDir, id, "drm", "renderD*"))
		for _, r := range renders {
			minor, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(r), "renderD"))
			if bdf, ok := renderBDF[minor]; ok && err == nil {
				// KFD spells the function as a fourth colon-separated field
				i := strings.LastIndex(bdf, ":")
				return bdf[:i] + "." + bdf[i+1:]
			}
		}
		return id
	}
}
