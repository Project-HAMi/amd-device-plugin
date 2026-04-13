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
package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ROCm/k8s-device-plugin/internal/pkg/allocator"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/amdgpu"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/exporter"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/utils"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/cuallocation"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"golang.org/x/net/context"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const defaultSplitCount = 10

// Plugin is identical to DevicePluginServer interface of device plugin API.
type AMDGPUPlugin struct {
	AMDGPUs            map[string]map[string]interface{}
	Heartbeat          chan bool
	signal             chan os.Signal
	disableWatchAndRegister    chan bool
	ackDisableWatchAndRegister chan bool
	deviceCache        []*utils.DeviceInfo
	Resource           string
	devAllocator       allocator.Policy
	allocatorInitError bool
	cuAllocation       map[string]cuallocation.Allocation // device ID -> cu allocation
}

type AMDGPUPluginOption func(*AMDGPUPlugin)

func NewAMDGPUPlugin(options ...AMDGPUPluginOption) *AMDGPUPlugin {
	amdGpuPlugin := &AMDGPUPlugin{}
	for _, option := range options {
		option(amdGpuPlugin)
	}
	return amdGpuPlugin
}

func WithAllocator(a allocator.Policy) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.devAllocator = a
	}
}

func WithHeartbeat(ch chan bool) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.Heartbeat = ch
	}
}
func WithResource(res string) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.Resource = res
	}
}

// Start is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after
// plugin instantiation and before its registration to kubelet. This
// method could be used to prepare resources before they are offered
// to Kubernetes.
func (p *AMDGPUPlugin) Start() error {
	p.signal = make(chan os.Signal, 1)
	if p.disableWatchAndRegister == nil {
		p.disableWatchAndRegister = make(chan bool, 1)
	}
	if p.ackDisableWatchAndRegister == nil {
		p.ackDisableWatchAndRegister = make(chan bool, 1)
	}
	signal.Notify(p.signal, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	err := p.devAllocator.Init(getDevices(), "")
	if err != nil {
		glog.Errorf("allocator init failed. Falling back to kubelet default allocation. Error %v", err)
		p.allocatorInitError = true
	}

	go func() {
		p.WatchAndRegister(p.disableWatchAndRegister, p.ackDisableWatchAndRegister)
	}()

	return nil
}

const (
	registerAnnosKey        = "amd.com/register-gpus"
	registerGPUPairScoreKey = "amd.com/gpu-pair-score"
)

func (p *AMDGPUPlugin) RegisterInAnnotation() error {
	devices := p.getAPIDevices()
	p.deviceCache = devices
	glog.Infof("start working on the devices: %v", devices)

	annos := make(map[string]string)
	nodeName := os.Getenv(utils.NodeNameEnvName)
	node, err := utils.GetNode(nodeName)
	if err != nil {
		glog.Errorf("get node error: %v", err)
		return err
	}

	glog.Infof("patch topo score to node: %s=%s", registerGPUPairScoreKey, marshalNodeDevices(p.deviceCache))
	annos[registerAnnosKey] = marshalNodeDevices(p.deviceCache)
	glog.Infof("patch node with the following annos %v", annos)
	err = utils.PatchNodeAnnotations(node, annos)
	if err != nil {
		glog.Errorf("patch node error: %v", err)
	}
	return err
}

func (p *AMDGPUPlugin) getAPIDevices() []*utils.DeviceInfo {
	p.AMDGPUs = amdgpu.GetAMDGPUs()

	// Node name is required to make device UUID unique cluster-wide.
	nodeName := os.Getenv(utils.NodeNameEnvName)
	if nodeName == "" {
		nodeName = "unknown-node"
	}

	// Best-effort: enrich devices with node-labeller labels (amd.com/gpu.* and beta.amd.com/gpu.*).
	var gpuNodeLabels map[string]string
	var vramMiB int32
	var cuCount int32
	deviceType := "amd-gpu"
	if nodeName != "unknown-node" {
		if node, err := utils.GetNode(nodeName); err == nil && node != nil {
			gpuNodeLabels = extractAMDGPUNodeLabels(node.Labels)

			// NOTE: node-labeller labels are node-scoped aggregations. When a node has GPUs with
			// different values (e.g., different product-name / vram / cu-count), the labeller
			// typically emits per-value counter labels like:
			//   amd.com/gpu.product-name.<VALUE>=<COUNT>
			// and may omit the singular label:
			//   amd.com/gpu.product-name=<VALUE>
			// Therefore the mapping below cannot reliably provide per-device attributes on a
			// heterogeneous node; it will either pick one node-level value or fall back.
			//
			// TODO: derive per-device productName/VRAM/CU from per-GPU sources (e.g. amdgpu sysfs/KFD
			// topology keyed by renderD/devID) instead of node labels.
			vramMiB = parseVRAMMiBFromLabels(gpuNodeLabels)
			cuCount = parseInt32FromLabels(gpuNodeLabels, "amd.com/gpu.cu-count", "beta.amd.com/gpu.cu-count")
			if v, ok := firstLabelValue(gpuNodeLabels, "amd.com/gpu.product-name", "beta.amd.com/gpu.product-name"); ok {
				deviceType = v
			}
		} else if err != nil {
			glog.V(4).Infof("get node labels failed (skip label enrichment): %v", err)
		}
	}

	keys := make([]string, 0, len(p.AMDGPUs))
	for key := range p.AMDGPUs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]*utils.DeviceInfo, 0, len(keys))
	for _, key := range keys {
		deviceData := p.AMDGPUs[key]

		renderD, _ := deviceData["renderD"].(int)
		card, _ := deviceData["card"].(int)
		devID, _ := deviceData["devID"].(string)
		nodeID, _ := deviceData["nodeId"].(int)
		numa, _ := deviceData["numaNode"].(int)
		computePT, _ := deviceData["computePartitionType"].(string)
		memoryPT, _ := deviceData["memoryPartitionType"].(string)

		// UUID format must include renderD + node name (Allocate mapping + unique in cluster).
		uuid := fmt.Sprintf("%s-renderD%d", nodeName, renderD)
		// Keep only required fields in DeviceInfo; avoid duplicating device/node details in CustomInfo.
		_ = card
		_ = devID
		_ = nodeID
		_ = computePT
		_ = memoryPT
		_ = gpuNodeLabels

		out = append(out, &utils.DeviceInfo{
			ID:           uuid,
			Index:        uint(renderD),
			Count:        defaultSplitCount,
			Devmem:       vramMiB,
			Devcore:      cuCount,
			Type:         deviceType,
			Numa:         numa,
			Mode:         "",
			Health:       true,
			DeviceVendor: "AMD",
			CustomInfo:   nil,
		})
	}

	return out
}

func firstLabelValue(labels map[string]string, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := labels[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func parseInt32FromLabels(labels map[string]string, keys ...string) int32 {
	v, ok := firstLabelValue(labels, keys...)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

// node-labeller formats vram label like "16G" (GiB, rounded).
// We map it into MiB for DeviceInfo.Devmem.
func parseVRAMMiBFromLabels(labels map[string]string) int32 {
	v, ok := firstLabelValue(labels, "amd.com/gpu.vram", "beta.amd.com/gpu.vram")
	if !ok {
		return 0
	}
	s := strings.TrimSpace(v)
	if strings.HasSuffix(strings.ToUpper(s), "G") {
		num := strings.TrimSpace(s[:len(s)-1])
		n, err := strconv.ParseInt(num, 10, 32)
		if err != nil {
			return 0
		}
		// GiB -> MiB
		return int32(n * 1024)
	}
	// Fallback: try plain integer (assume MiB)
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

func extractAMDGPUNodeLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	const (
		amdPrefix  = "amd.com/gpu."
		betaPrefix = "beta.amd.com/gpu."
	)
	out := make(map[string]string)
	for k, v := range labels {
		if strings.HasPrefix(k, amdPrefix) || strings.HasPrefix(k, betaPrefix) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func marshalNodeDevices(devices []*utils.DeviceInfo) string {
	b, err := json.Marshal(devices)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func getDevicesUUIDList(devices []*utils.DeviceInfo) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		if d == nil {
			continue
		}
		out = append(out, d.ID)
	}
	return out
}

func (p *AMDGPUPlugin) WatchAndRegister(disableWatchAndRegister <-chan bool, ackDisableWatchAndRegister chan<- bool) {
	glog.Info("Starting WatchAndRegister")
	errorSleepInterval := 5 * time.Second
	successSleepInterval := 30 * time.Second
	var disabled bool

	for {
		select {
		case disable := <-disableWatchAndRegister:
			if disable {
				glog.Info("Received disable signal, stopping WatchAndRegister")
				disabled = true
			} else {
				glog.Info("Received enable signal, resuming WatchAndRegister")
				disabled = false
			}
		default:
		}

		if disabled {
			glog.Info("WatchAndRegister is disabled, sleep success interval")
			ackDisableWatchAndRegister <- true
			time.Sleep(successSleepInterval)
			continue
		}

		if err := p.RegisterInAnnotation(); err != nil {
			glog.Errorf("Failed to register annotation: %v", err)
			glog.Infof("Retrying in %v...", errorSleepInterval)
			time.Sleep(errorSleepInterval)
		} else {
			glog.Infof("Successfully registered annotation. Next check in %v...", successSleepInterval)
			time.Sleep(successSleepInterval)
		}
	}
}

func getDevices() []*allocator.Device {
	devices := amdgpu.GetAMDGPUs()
	var deviceList []*allocator.Device

	for id, deviceData := range devices {
		device := &allocator.Device{
			Id:                   id,
			Card:                 deviceData["card"].(int),
			RenderD:              deviceData["renderD"].(int),
			DevId:                deviceData["devID"].(string),
			ComputePartitionType: deviceData["computePartitionType"].(string),
			MemoryPartitionType:  deviceData["memoryPartitionType"].(string),
			NodeId:               deviceData["nodeId"].(int),
			NumaNode:             deviceData["numaNode"].(int),
		}
		deviceList = append(deviceList, device)
	}
	return deviceList
}

// Stop is an optional interface that could be implemented by plugin.
// If case Stop is implemented, it will be executed by Manager after the
// plugin is unregistered from kubelet. This method could be used to tear
// down resources.
func (p *AMDGPUPlugin) Stop() error {
	return nil
}

var topoSIMDre = regexp.MustCompile(`simd_count\s(\d+)`)

func countGPUDevFromTopology(topoRootParam ...string) int {
	topoRoot := "/sys/class/kfd/kfd"
	if len(topoRootParam) == 1 {
		topoRoot = topoRootParam[0]
	}

	count := 0
	var nodeFiles []string
	var err error
	if nodeFiles, err = filepath.Glob(topoRoot + "/topology/nodes/*/properties"); err != nil {
		glog.Fatalf("glob error: %s", err)
		return count
	}

	for _, nodeFile := range nodeFiles {
		glog.Info("Parsing " + nodeFile)
		f, e := os.Open(nodeFile)
		if e != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			m := topoSIMDre.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}

			if v, _ := strconv.Atoi(m[1]); v > 0 {
				count++
				break
			}
		}
		f.Close()
	}
	return count
}

func simpleHealthCheck() bool {
	entries, err := filepath.Glob("/sys/class/kfd/kfd/topology/nodes/*/properties")
	if err != nil {
		glog.Errorf("Error finding properties files: %v", err)
		return false
	}

	for _, propFile := range entries {
		f, err := os.Open(propFile)
		if err != nil {
			glog.Errorf("Error opening %s: %v", propFile, err)
			continue
		}
		defer f.Close()

		var cpuCores, gfxVersion int
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "cpu_cores_count") {
				parts := strings.Fields(line)
				if len(parts) == 2 {
					cpuCores, _ = strconv.Atoi(parts[1])
				}
			} else if strings.HasPrefix(line, "gfx_target_version") {
				parts := strings.Fields(line)
				if len(parts) == 2 {
					gfxVersion, _ = strconv.Atoi(parts[1])
				}
			}
		}

		if err := scanner.Err(); err != nil {
			glog.Warningf("Error scanning %s: %v", propFile, err)
			continue
		}

		if cpuCores == 0 && gfxVersion > 0 {
			// Found a GPU
			return true
		}
	}

	glog.Warning("No GPU nodes found via properties")
	return false
}

// GetDevicePluginOptions returns options to be communicated with Device
// Manager
func (p *AMDGPUPlugin) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	if p.allocatorInitError {
		return &pluginapi.DevicePluginOptions{}, nil
	}
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: true,
	}, nil
}

// PreStartContainer is expected to be called before each container start if indicated by plugin during registration phase.
// PreStartContainer allows kubelet to pass reinitialized devices to containers.
// PreStartContainer allows Device Plugin to run device specific operations on the Devices requested
func (p *AMDGPUPlugin) PreStartContainer(ctx context.Context, r *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (p *AMDGPUPlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {

	p.AMDGPUs = amdgpu.GetAMDGPUs()

	glog.Infof("Found %d AMDGPUs", len(p.AMDGPUs))

	devs := make([]*pluginapi.Device, len(p.AMDGPUs))
	var isHomogeneous bool
	isHomogeneous = amdgpu.IsHomogeneous()
	// Initialize a map to store partitionType based device list
	resourceTypeDevs := make(map[string][]*pluginapi.Device)

	if isHomogeneous {
		// limit scope for hwloc
		func() {
			i := 0
			for id, device := range p.AMDGPUs {
				dev := &pluginapi.Device{
					ID:     id,
					Health: pluginapi.Healthy,
				}
				devs[i] = dev
				i++

				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				dev.Topology = &pluginapi.TopologyInfo{
					Nodes: numaNodes,
				}
			}
		}()
		s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	} else {
		func() {
			for id, device := range p.AMDGPUs {
				dev := &pluginapi.Device{
					ID:     id,
					Health: pluginapi.Healthy,
				}
				// Append a device belonging to a certain partition type to its respective list
				partitionType := device["computePartitionType"].(string) + "_" + device["memoryPartitionType"].(string)
				resourceTypeDevs[partitionType] = append(resourceTypeDevs[partitionType], dev)

				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				dev.Topology = &pluginapi.TopologyInfo{
					Nodes: numaNodes,
				}
			}
		}()
		// Send the appropriate list of devices based on the partitionType
		if devList, exists := resourceTypeDevs[p.Resource]; exists {
			s.Send(&pluginapi.ListAndWatchResponse{Devices: devList})
		}
	}

loop:
	for {
		select {
		case <-p.Heartbeat:
			var health = pluginapi.Unhealthy

			if simpleHealthCheck() {
				health = pluginapi.Healthy
			}

			// update with per device GPU health status
			if isHomogeneous {
				exporter.PopulatePerGPUDHealth(devs, health)
				s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
			} else {
				if devList, exists := resourceTypeDevs[p.Resource]; exists {
					exporter.PopulatePerGPUDHealth(devList, health)
					s.Send(&pluginapi.ListAndWatchResponse{Devices: devList})
				}
			}

		case <-p.signal:
			glog.Infof("Received signal, exiting")
			break loop
		}
	}
	// returning a value with this function will unregister the plugin from k8s

	return nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate
// from a list of available ones. The resulting preferred allocation is not
// guaranteed to be the allocation ultimately performed by the
// devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (p *AMDGPUPlugin) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	response := &pluginapi.PreferredAllocationResponse{}
	for _, req := range req.ContainerRequests {
		allocated_ids, err := p.devAllocator.Allocate(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
		if err != nil {
			glog.Errorf("unable to get preferred allocation list. Error:%v", err)
			return nil, fmt.Errorf("unable to get preferred allocation list. Error:%v", err)
		}
		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: allocated_ids,
		}
		response.ContainerResponses = append(response.ContainerResponses, resp)
	}
	return response, nil
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (p *AMDGPUPlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse
	var car pluginapi.ContainerAllocateResponse
	var dev *pluginapi.DeviceSpec

	err := p.syncCUAllocation()
	if err != nil {
		glog.Errorf("unable to sync cu allocation: %v", err)
		return nil, fmt.Errorf("unable to sync cu allocation: %v", err)
	}
	for _, req := range r.ContainerRequests {
		car = pluginapi.ContainerAllocateResponse{}

		// Currently, there are only 1 /dev/kfd per nodes regardless of the # of GPU available
		// for compute/rocm/HSA use cases
		dev = new(pluginapi.DeviceSpec)
		dev.HostPath = "/dev/kfd"
		dev.ContainerPath = "/dev/kfd"
		dev.Permissions = "rw"
		car.Devices = append(car.Devices, dev)

		for _, id := range req.DevicesIDs {
			glog.Infof("Allocating device ID: %s", id)

			for k, v := range p.AMDGPUs[id] {
				// Map struct previously only had 'card' and 'renderD' and only those are paths to be appended as before
				if k != "card" && k != "renderD" {
					continue
				}
				devpath := fmt.Sprintf("/dev/dri/%s%d", k, v)
				dev = new(pluginapi.DeviceSpec)
				dev.HostPath = devpath
				dev.ContainerPath = devpath
				dev.Permissions = "rw"
				car.Devices = append(car.Devices, dev)
			}
		}

		response.ContainerResponses = append(response.ContainerResponses, &car)
	}

	return &response, nil
}

// syncCUAllocation syncs the cu allocation from the node pods to the plugin
func (p *AMDGPUPlugin) syncCUAllocation() error {
	return nil
}

// Lister serves as an interface between imlementation and Manager machinery. User passes
// implementation of this interface to NewManager function. Manager will use it to obtain resource
// namespace, monitor available resources and instantate a new plugin for them.
type AMDGPULister struct {
	ResUpdateChan chan dpm.PluginNameList
	Heartbeat     chan bool
	Signal        chan os.Signal
}

// GetResourceNamespace must return namespace (vendor ID) of implemented Lister. e.g. for
// resources in format "color.example.com/<color>" that would be "color.example.com".
func (l *AMDGPULister) GetResourceNamespace() string {
	return "amd.com"
}

// Discover notifies manager with a list of currently available resources in its namespace.
// e.g. if "color.example.com/red" and "color.example.com/blue" are available in the system,
// it would pass PluginNameList{"red", "blue"} to given channel. In case list of
// resources is static, it would use the channel only once and then return. In case the list is
// dynamic, it could block and pass a new list each times resources changed. If blocking is
// used, it should check whether the channel is closed, i.e. Discover should stop.
func (l *AMDGPULister) Discover(pluginListCh chan dpm.PluginNameList) {
	for {
		select {
		case newResourcesList := <-l.ResUpdateChan: // New resources found
			pluginListCh <- newResourcesList
		case <-pluginListCh: // Stop message received
			// Stop resourceUpdateCh
			return
		}
	}
}

// NewPlugin instantiates a plugin implementation. It is given the last name of the resource,
// e.g. for resource name "color.example.com/red" that would be "red". It must return valid
// implementation of a PluginInterface.
func (l *AMDGPULister) NewPlugin(resourceLastName string) dpm.PluginInterface {
	options := []AMDGPUPluginOption{
		WithHeartbeat(l.Heartbeat),
		WithResource(resourceLastName),
		WithAllocator(allocator.NewBestEffortPolicy()),
	}
	return NewAMDGPUPlugin(options...)
}
