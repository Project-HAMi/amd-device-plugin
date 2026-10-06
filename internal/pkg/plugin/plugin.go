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
	"context"
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

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/allocator"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdsmi"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/dmem"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/exporter"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/libcheck"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// splitCount is how many workloads may share one GPU. Set once at startup
// from AMDGPULister.SplitCount; gfx12 GPUs contend above about 2 (see README).
var splitCount = 10

// Plugin is identical to DevicePluginServer interface of device plugin API.
type AMDGPUPlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	AMDGPUs                    map[string]map[string]interface{}
	Heartbeat                  chan bool
	signal                     chan os.Signal
	disableWatchAndRegister    chan bool
	ackDisableWatchAndRegister chan bool
	deviceCache                []*utils.DeviceInfo
	Resource                   string
	devAllocator               allocator.Policy
	// cdiSpecDir, when set, makes Allocate hand out CDI devices described
	// by a spec written there instead of raw device nodes.
	cdiSpecDir         string
	allocatorInitError bool
	// amdSMIUUIDToTopology maps the stable AMD SMI device UUID published in
	// DeviceInfo.ID back to the local topology key required during Allocate.
	amdSMIUUIDToTopology map[string]string
	// amdSMIUUIDToROCrUUID maps the scheduler-facing AMD SMI UUID to the UUID
	// spelling understood by ROCR_VISIBLE_DEVICES.
	amdSMIUUIDToROCrUUID map[string]string
	// bdfToROCrUUID maps the topology BDF to its ROCr UUID for kubelet split
	// ids ("<bdf>#<slot>") resolved during Allocate.
	bdfToROCrUUID map[string]string
	// dmemEnabled additionally caps sliced allocations through the kernel
	// dmem cgroup controller when available, a hard driver-level VRAM cap
	// that needs no LD_AUDIT injection (see Project-HAMi/amd-hami-core#6).
	// A failure to set it is logged and never fails Allocate; libamvgpu's
	// HIP_DEVICE_MEMORY_LIMIT_<i> remains the enforcement path either way.
	dmemEnabled bool
	// libcheckInspector, when non-nil, makes Allocate refuse a sliced
	// allocation whose container image is not LD_AUDIT-compatible (musl,
	// or no dynamic loader at all), instead of silently running it
	// unprotected (Project-HAMi/amd-hami-core#3). Unlike dmemEnabled this
	// check is synchronous: it must complete before Allocate returns, or
	// the container could already be running by the time it is.
	libcheckInspector *libcheck.Inspector
}

type AMDGPUPluginOption func(*AMDGPUPlugin)

func NewAMDGPUPlugin(options ...AMDGPUPluginOption) *AMDGPUPlugin {
	amdGpuPlugin := &AMDGPUPlugin{}
	for _, option := range options {
		option(amdGpuPlugin)
	}
	return amdGpuPlugin
}

// WithCDISpecDir enables CDI device injection with specs written to dir.
func WithCDISpecDir(dir string) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.cdiSpecDir = dir
	}
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

// WithDmemBackend enables the dmem cgroup VRAM cap for sliced allocations.
func WithDmemBackend(enabled bool) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.dmemEnabled = enabled
	}
}

// WithMuslFailClosed enables refusing sliced allocations on an
// LD_AUDIT-incompatible image. ctrPath and containerdSocket must resolve
// inside the plugin's own container (see internal/pkg/libcheck).
func WithMuslFailClosed(enabled bool, ctrPath, containerdSocket string) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		if !enabled {
			return
		}
		p.libcheckInspector = &libcheck.Inspector{
			CtrPath:          ctrPath,
			ContainerdSocket: containerdSocket,
			Namespace:        "k8s.io",
		}
	}
}

// Start is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after
// plugin instantiation and before its registration to kubelet. This
// method could be used to prepare resources before they are offered
// to Kubernetes.
func (p *AMDGPUPlugin) Start() error {
	if utils.GetClient() == nil {
		utils.InitGlobalClient()
	}
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

	// Initialize deviceCache before Allocate rebuilds CU occupancy from Pod annotations.
	if err := p.RegisterInAnnotation(); err != nil {
		return fmt.Errorf("initialize device cache: %w", err)
	}

	return nil
}

const (
	registerAnnosKey = "hami.io/node-amd-register"
	NodeLockName     = "hami.io/mutex.lock"
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

	// Resolve a stable per-device UUID through the AMD SMI C API. Unlike the
	// node labeller, this is not an aggregated node-level value and supports
	// SR-IOV virtual functions.
	bdfs := make([]string, 0, len(p.AMDGPUs))
	for _, deviceData := range p.AMDGPUs {
		if bdf, ok := deviceData["devID"].(string); ok {
			bdfs = append(bdfs, bdf)
		}
	}
	amdSMIUUIDs, err := amdsmi.GetAMDSMIUUIDs(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI UUID lookup incomplete; GPUs without an AMD SMI UUID will not be registered: %v", err)
	}
	p.amdSMIUUIDToTopology = make(map[string]string, len(amdSMIUUIDs))
	p.amdSMIUUIDToROCrUUID = make(map[string]string, len(amdSMIUUIDs))
	p.bdfToROCrUUID = make(map[string]string, len(p.AMDGPUs))
	rocrUUIDs := amdgpu.GetROCrUUIDsFromTopology()
	amdSMIProductNames, err := amdsmi.GetAMDSMIProductNames(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI product-name lookup incomplete; using amd-gpu where necessary: %v", err)
	}

	keys := make([]string, 0, len(p.AMDGPUs))
	for key := range p.AMDGPUs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]*utils.DeviceInfo, 0, len(keys))
	for _, key := range keys {
		deviceData := p.AMDGPUs[key]

		card, _ := deviceData["card"].(int)
		numa, _ := deviceData["numaNode"].(int)
		capacity, capacityErr := amdgpu.GetDeviceCapacity(fmt.Sprintf("card%d", card))
		if capacityErr != nil {
			glog.Warningf("libdrm capacity lookup failed for GPU %s (card%d): %v", key, card, capacityErr)
		}

		bdf, ok := deviceData["devID"].(string)
		if !ok || bdf == "" {
			glog.Errorf("skip GPU %s: missing PCI BDF in topology", key)
			continue
		}
		uuid, found := amdSMIUUIDs[strings.ToLower(bdf)]
		if !found || uuid == "" {
			// Do not fall back to a node/BDF-derived ID. The scheduler must only
			// see stable, hardware-bound AMD SMI UUIDs.
			glog.Errorf("skip GPU %s: AMD SMI did not return a UUID for topology BDF %s", key, bdf)
			continue
		}
		renderD, ok := deviceData["renderD"].(int)
		if !ok || renderD <= 0 {
			glog.Errorf("skip GPU %s: missing or invalid render node in topology", key)
			continue
		}
		rocrUUID := rocrUUIDs[renderD]
		if rocrUUID == "" {
			// APUs report KFD unique_id 0; Allocate addresses them by index.
			glog.Warningf("GPU %s has no ROCr UUID for renderD%d; using its container-local index", key, renderD)
		}
		// Keep BDF in the annotation for consumers that need to locate the
		// device node. Allocate uses the in-memory UUID -> topology map below.
		p.amdSMIUUIDToTopology[uuid] = key
		p.amdSMIUUIDToROCrUUID[uuid] = rocrUUID
		p.bdfToROCrUUID[key] = rocrUUID
		// key is the standard PCI BDF spelling (domain:bus:device.function).
		// The KFD topology bdf above uses a fourth colon-separated component.
		customInfo := map[string]any{"pciBDF": strings.ToLower(key)}
		nodeId, _ := deviceData["nodeId"].(int)
		if q, ok := computeQueues(kfdTopologyNodes, nodeId); ok {
			customInfo["computeQueues"] = q
		}
		deviceType := "amd-gpu"
		if productName, ok := amdSMIProductNames[strings.ToLower(bdf)]; ok && productName != "" {
			deviceType = productName
		}

		out = append(out, &utils.DeviceInfo{
			ID:           uuid,
			Index:        uint(card),
			Count:        int32(splitCount),
			Devmem:       capacity.VRAMMiB,
			Devcore:      capacity.CUCount,
			Type:         deviceType,
			Numa:         numa,
			Mode:         "",
			Health:       true,
			DeviceVendor: "amd",
			CustomInfo:   customInfo,
		})
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
		for splitIdx := 0; splitIdx < splitCount; splitIdx++ {
			device := &allocator.Device{
				Id:                   fmt.Sprintf("%s#%d", id, splitIdx),
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
	if p.cdiSpecDir != "" {
		if err := writeCDISpec(p.cdiSpecDir, p.AMDGPUs); err != nil {
			glog.Errorf("write CDI spec to %s: %v; falling back to device nodes", p.cdiSpecDir, err)
			p.cdiSpecDir = ""
		}
	}

	devs := make([]*pluginapi.Device, 0, len(p.AMDGPUs)*splitCount)
	isHomogeneous := amdgpu.IsHomogeneous()
	// Initialize a map to store partitionType based device list
	resourceTypeDevs := make(map[string][]*pluginapi.Device)

	if isHomogeneous {
		// limit scope for hwloc
		func() {
			for id, device := range p.AMDGPUs {
				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				for splitIdx := 0; splitIdx < splitCount; splitIdx++ {
					dev := &pluginapi.Device{
						ID:     fmt.Sprintf("%s#%d", id, splitIdx),
						Health: pluginapi.Healthy,
						Topology: &pluginapi.TopologyInfo{
							Nodes: numaNodes,
						},
					}
					devs = append(devs, dev)
				}
			}
		}()
		s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	} else {
		func() {
			for id, device := range p.AMDGPUs {
				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				partitionType := device["computePartitionType"].(string) + "_" + device["memoryPartitionType"].(string)
				for splitIdx := 0; splitIdx < splitCount; splitIdx++ {
					dev := &pluginapi.Device{
						ID:     fmt.Sprintf("%s#%d", id, splitIdx),
						Health: pluginapi.Healthy,
						Topology: &pluginapi.TopologyInfo{
							Nodes: numaNodes,
						},
					}
					// Append a split device belonging to this partition type.
					resourceTypeDevs[partitionType] = append(resourceTypeDevs[partitionType], dev)
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
			// exporter state (ECC, throttle) where available, then a GPU whose
			// DRM device cannot be opened is unhealthy regardless
			if isHomogeneous {
				exporter.PopulatePerGPUDHealth(devs, pluginapi.Healthy)
				p.markNonFunctional(devs, amdgpu.DevFunctional)
				s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
			} else {
				if devList, exists := resourceTypeDevs[p.Resource]; exists {
					exporter.PopulatePerGPUDHealth(devList, pluginapi.Healthy)
					p.markNonFunctional(devList, amdgpu.DevFunctional)
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

// markNonFunctional marks every split of a GPU unhealthy when functional
// reports that its DRM card cannot be opened, or the GPU is no longer known.
func (p *AMDGPUPlugin) markNonFunctional(devs []*pluginapi.Device, functional func(card string) bool) {
	ok := map[string]bool{}
	for _, d := range devs {
		id := strings.SplitN(d.ID, "#", 2)[0]
		f, seen := ok[id]
		if !seen {
			card, known := p.AMDGPUs[id]["card"].(int)
			f = known && functional(fmt.Sprintf("card%d", card))
			ok[id] = f
		}
		if !f {
			d.Health = pluginapi.Unhealthy
		}
	}
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

// deviceDataFromAllocationUUID resolves the AMD SMI UUID published in
// DeviceInfo.ID to the local topology required to prepare a container.
func (p *AMDGPUPlugin) deviceDataFromAllocationUUID(uuid, _ string) (map[string]interface{}, error) {
	if topoKey, ok := p.amdSMIUUIDToTopology[uuid]; ok {
		if d, found := p.AMDGPUs[topoKey]; found {
			return d, nil
		}
		return nil, fmt.Errorf("AMD SMI UUID %q resolves to unavailable topology key %q", uuid, topoKey)
	}
	// kubelet split ids are "<bdf>#<slot>"; resolve the bare BDF directly.
	if bdf := strings.SplitN(uuid, "#", 2)[0]; bdf != uuid {
		if d, found := p.AMDGPUs[bdf]; found {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no local GPU topology entry for AMD SMI UUID %q", uuid)
}

func (p *AMDGPUPlugin) rocrUUIDFromAllocationUUID(uuid string) (string, error) {
	// An empty ROCr UUID is a known GPU without one, see rocrVisibleList.
	if rocrUUID, ok := p.amdSMIUUIDToROCrUUID[uuid]; ok {
		return rocrUUID, nil
	}
	if bdf := strings.SplitN(uuid, "#", 2)[0]; bdf != uuid {
		if rocrUUID, ok := p.bdfToROCrUUID[bdf]; ok {
			return rocrUUID, nil
		}
	}
	return "", fmt.Errorf("no ROCr UUID for AMD SMI UUID %q", uuid)
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (p *AMDGPUPlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	response := &pluginapi.AllocateResponse{}

	// resolve device allocation from annotation
	glog.Infof("Allocate request: %+v", r)
	nodename := os.Getenv(utils.NodeNameEnvName)
	current, err := utils.GetPendingPod(ctx, nodename)
	if err != nil {
		return &pluginapi.AllocateResponse{}, err
	}
	podDevices := current.Annotations[utils.DeviceAllocation]
	podCuAllocList := map[string]string{}
	if raw := strings.TrimSpace(current.Annotations[utils.CuAllocation]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &podCuAllocList); err != nil {
			utils.PodAllocationFailed(nodename, current, NodeLockName)
			return &pluginapi.AllocateResponse{}, fmt.Errorf("parse existing %s: %w", utils.CuAllocation, err)
		}
	}
	// Pod annotations are the source of truth. The scheduler keeps the node
	// locked until this allocation is persisted, so a direct API read gives us
	// the complete bitmap committed by all preceding Pods without depending on
	// informer delivery.
	cuAllocationSnapshot, err := p.rebuildCUAllocations(ctx, nodename)
	if err != nil {
		utils.PodAllocationFailed(nodename, current, NodeLockName)
		return &pluginapi.AllocateResponse{}, fmt.Errorf("rebuild CU allocation for node %s: %w", nodename, err)
	}
	hostHookPath := os.Getenv("HOST_HOOK_PATH")
	glog.Infof("Allocate pod name is %s/%s, annotation is %+v", current.Namespace, current.Name, current.Annotations)

	// True when any container requests a core or memory slice; the CU
	// persistence and node lock guard shared occupancy only.
	slicedCU := false
	for idx := range r.ContainerRequests {
		currentCtr, devreq, err := utils.GetNextDeviceRequest("amd", *current)
		glog.Infof("deviceAllocateFromAnnotation(container=%s)=%+v", currentCtr.Name, devreq)
		if err != nil || len(devreq) != len(r.ContainerRequests[idx].DevicesIds) {
			// The fork scheduler's annotation key is not the one this plugin
			// reads; fall back to kubelet's own allocation (whole GPU).
			glog.Warningf("annotation allocation unavailable (%v); using kubelet device ids %v", err, r.ContainerRequests[idx].DevicesIds)
			devreq = utils.ContainerDevices{}
			for _, id := range r.ContainerRequests[idx].DevicesIds {
				devreq = append(devreq, utils.ContainerDevice{UUID: id, Type: "AMDGPU", Usedcores: 0})
			}
		} else {
			err = utils.EraseNextDeviceTypeFromAnnotation("amd", *current)
			if err != nil {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, err
			}
		}

		car := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{},
		}
		rocrVisibleDevices := make([]string, 0, len(devreq))
		renderMinors := make([]int, 0, len(devreq))

		// KFD + DRI from annotation UUID topology; with CDI the spec carries them.
		if p.cdiSpecDir == "" {
			car.Devices = append(car.Devices, &pluginapi.DeviceSpec{
				HostPath:      "/dev/kfd",
				ContainerPath: "/dev/kfd",
				Permissions:   "rw",
			})
		}
		for _, d := range devreq {
			glog.Infof("Allocating device from annotation UUID: %s", d.UUID)
			deviceData, topoErr := p.deviceDataFromAllocationUUID(d.UUID, nodename)
			if topoErr != nil {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, topoErr
			}
			cardMinor, cok := deviceData["card"].(int)
			renderMinor, rok := deviceData["renderD"].(int)
			if !cok || !rok {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, fmt.Errorf("invalid card/renderD in topology for UUID %q", d.UUID)
			}
			rocrUUID, rocrErr := p.rocrUUIDFromAllocationUUID(d.UUID)
			if rocrErr != nil {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, rocrErr
			}
			rocrVisibleDevices = append(rocrVisibleDevices, rocrUUID)
			renderMinors = append(renderMinors, renderMinor)
			if p.cdiSpecDir != "" {
				car.CdiDevices = append(car.CdiDevices, &pluginapi.CDIDevice{Name: cdiDeviceName(cardMinor)})
				continue
			}
			for _, pair := range []struct {
				kind  string
				minor int
			}{
				{"card", cardMinor},
				{"renderD", renderMinor},
			} {
				devpath := fmt.Sprintf("/dev/dri/%s%d", pair.kind, pair.minor)
				car.Devices = append(car.Devices, &pluginapi.DeviceSpec{
					HostPath:      devpath,
					ContainerPath: devpath,
					Permissions:   "rw",
				})
			}
		}

		if len(devreq) > 0 {
			hsaCuSets := make([]string, 0, len(devreq))
			for _, d := range devreq {
				if d.UUID == "" {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, fmt.Errorf("empty device uuid in allocation request")
				}
			}

			for i, d := range devreq {
				totalCUs, err := p.getDeviceTotalCUs(d.UUID)
				if err != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, err
				}
				baseAllocation, ok := cuAllocationSnapshot[d.UUID]
				if !ok {
					baseAllocation, err = cuallocation.NewAllocation(totalCUs)
					if err != nil {
						utils.PodAllocationFailed(nodename, current, NodeLockName)
						return &pluginapi.AllocateResponse{}, err
					}
					cuAllocationSnapshot[d.UUID] = baseAllocation
				}
				cores := int(d.Usedcores)
				if cores == 0 {
					// Whole GPU: mask every CU.
					cores = totalCUs
				}
				_, deltaAllocation, err := cuallocation.AllocateN(baseAllocation, totalCUs, cores, p.cuMaskUnit(d.UUID, kfdTopologyNodes))
				if err != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, fmt.Errorf("allocate cu for %s: %w", d.UUID, err)
				}
				cuList := allocationToIDList(deltaAllocation, totalCUs)
				// HSA_CU_MASK: GPU_list:CU_list[;GPU_list:CU_list]*.
				// Use container-local device index as GPU_list and ID_List as CU_list.
				hsaCuSets = append(hsaCuSets, fmt.Sprintf("%d:%s", i, cuList))

				slicedCU = slicedCU || !p.isWholeGPU(utils.ContainerDevices{d})
				if oldList, ok := podCuAllocList[d.UUID]; ok && strings.TrimSpace(oldList) != "" {
					oldAllocation, err := idListToAllocation(oldList, totalCUs)
					if err != nil {
						utils.PodAllocationFailed(nodename, current, NodeLockName)
						return &pluginapi.AllocateResponse{}, fmt.Errorf("decode existing pod cu allocation for %s: %w", d.UUID, err)
					}
					mergedAllocation, err := cuallocation.AddAllocation(oldAllocation, totalCUs, deltaAllocation)
					if err != nil {
						utils.PodAllocationFailed(nodename, current, NodeLockName)
						return &pluginapi.AllocateResponse{}, fmt.Errorf("merge pod cu allocation for %s: %w", d.UUID, err)
					}
					podCuAllocList[d.UUID] = allocationToIDList(mergedAllocation, totalCUs)
				} else {
					podCuAllocList[d.UUID] = cuList
				}
			}
			car.Envs["HSA_CU_MASK"] = strings.Join(hsaCuSets, ";")
			// ROCr renumbers devices after this list is applied. HSA_CU_MASK uses
			// those container-local indices, so it is built in the same order.
			car.Envs["ROCR_VISIBLE_DEVICES"] = rocrVisibleList(rocrVisibleDevices, renderMinors)
			// HIP/Ray/PyTorch read HIP_VISIBLE_DEVICES (CUDA-compatible semantics).
			// Ray errors if only ROCR_VISIBLE_DEVICES is set; inject matching
			// container-local indices so both layers agree after ROCr renumbering.
			hipVisibleDevices := make([]string, len(rocrVisibleDevices))
			for i := range rocrVisibleDevices {
				hipVisibleDevices[i] = strconv.Itoa(i)
			}
			car.Envs["HIP_VISIBLE_DEVICES"] = strings.Join(hipVisibleDevices, ",")
			// The hook only enforces slices and needs glibc >= 2.34, so whole-GPU
			// requests must not depend on it.
			if !p.isWholeGPU(devreq) {
				if p.libcheckInspector != nil {
					libc, checkErr := p.libcheckInspector.Inspect(ctx, currentCtr.Image)
					if checkErr != nil {
						utils.PodAllocationFailed(nodename, current, NodeLockName)
						return &pluginapi.AllocateResponse{}, fmt.Errorf("verify LD_AUDIT compatibility of image %q: %w", currentCtr.Image, checkErr)
					}
					if !libc.AuditCompatible() {
						utils.PodAllocationFailed(nodename, current, NodeLockName)
						return &pluginapi.AllocateResponse{}, fmt.Errorf("image %q uses %s, not glibc; LD_AUDIT (and so the memory/CU-slice limit) would silently not apply, refusing to start unprotected", currentCtr.Image, libc)
					}
				}
				// libamvgpu reads one limit per container-local device index.
				for i, d := range devreq {
					car.Envs[fmt.Sprintf("HIP_DEVICE_MEMORY_LIMIT_%d", i)] = fmt.Sprintf("%vm", d.Usedmem)
				}
				car.Envs["LD_AUDIT"] = "/usr/local/vgpu/libamvgpu.so"
				if p.dmemEnabled {
					// The pod's cgroup can take several seconds to appear
					// after binding (measured up to ~7s on a real RKE2
					// node), well past what Allocate should ever block for.
					// Apply the cap in the background; libamvgpu's
					// HIP_DEVICE_MEMORY_LIMIT_<i> above is already the
					// synchronous enforcement path.
					podNamespace, podName, podUID, qos := current.Namespace, current.Name, string(current.UID), current.Status.QOSClass
					d := devreq[0]
					go p.applyDmemCap(podNamespace, podName, podUID, qos, d)
				}
			}
		}

		car.Mounts = append(car.Mounts,
			&pluginapi.Mount{
				ContainerPath: "/usr/local/vgpu/libamvgpu.so",
				HostPath:      hostHookPath + "/vgpu/libamvgpu.so",
				ReadOnly:      true,
			},
		)

		response.ContainerResponses = append(response.ContainerResponses, &car)
	}

	// Whole-GPU allocations own the full card; their CU occupancy is not
	// shared, so there is nothing to persist and no node lock to check.
	if len(podCuAllocList) > 0 && slicedCU {
		b, err := json.Marshal(podCuAllocList)
		if err != nil {
			utils.PodAllocationFailed(nodename, current, NodeLockName)
			return &pluginapi.AllocateResponse{}, fmt.Errorf("marshal %s: %w", utils.CuAllocation, err)
		}
		if err := p.validateNodeLockOwner(ctx, nodename, current); err != nil {
			utils.PodAllocationFailed(nodename, current, NodeLockName)
			return &pluginapi.AllocateResponse{}, err
		}
		if err := utils.PatchPodAnnotations(current, map[string]string{utils.CuAllocation: string(b)}); err != nil {
			// A transport error can be returned after the API server applied the
			// patch. Read back before failing to preserve a durable commit.
			persisted, getErr := p.hasPersistedCUAllocation(ctx, current, string(b))
			if getErr == nil && persisted {
				glog.Warningf("CU allocation annotation patch for %s/%s returned %v after it was persisted", current.Namespace, current.Name, err)
			} else {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				if getErr != nil {
					return &pluginapi.AllocateResponse{}, fmt.Errorf("patch pod %s annotation: %w (and failed to confirm persisted state: %v)", utils.CuAllocation, err, getErr)
				}
				return &pluginapi.AllocateResponse{}, fmt.Errorf("patch pod %s annotation: %w", utils.CuAllocation, err)
			}
		}
	}

	glog.Infoln("Allocate Response", response.ContainerResponses)
	utils.PodAllocationTrySuccess(nodename, podDevices, NodeLockName, current)

	return response, nil
}

// applyDmemCap caps d's device at the kernel dmem cgroup controller, a hard
// driver-level VRAM limit independent of libamvgpu (see internal/pkg/dmem).
// Any failure (controller unavailable, GPU not registered, pod cgroup not
// yet created, wrong cgroup driver) is logged and never fails Allocate;
// libamvgpu's HIP_DEVICE_MEMORY_LIMIT_<i> is the enforcement path regardless.
// applyDmemCap runs in its own goroutine, outside Allocate's response path:
// the pod cgroup this writes to can take several seconds to appear after
// binding, far more than Allocate should ever block a container on.
func (p *AMDGPUPlugin) applyDmemCap(podNamespace, podName, podUID string, qos corev1.PodQOSClass, d utils.ContainerDevice) {
	if !dmem.Available(dmem.DefaultCgroupRoot) {
		return
	}
	deviceData, err := p.deviceDataFromAllocationUUID(d.UUID, "")
	if err != nil {
		glog.Warningf("dmem: resolve topology for %s: %v", d.UUID, err)
		return
	}
	bdf, ok := deviceData["devID"].(string)
	if !ok || bdf == "" {
		glog.Warningf("dmem: missing PCI BDF for %s", d.UUID)
		return
	}
	bdf = dmem.NormalizeBDF(bdf)
	region, ok := dmem.Region(dmem.DefaultCgroupRoot, bdf)
	if !ok {
		glog.Infof("dmem: no VRAM region registered for %s (%s); skipping cap", d.UUID, bdf)
		return
	}
	podCgroup := dmem.PodCgroupPath(dmem.DefaultCgroupRoot, podUID, qos)
	limitBytes := int64(d.Usedmem) * 1024 * 1024
	if err := dmem.SetMaxWithRetry(podCgroup, region, limitBytes); err != nil {
		glog.Warningf("dmem: cap %s at %d bytes on %s: %v", region, limitBytes, podCgroup, err)
		return
	}
	glog.Infof("dmem: capped %s at %d bytes on pod %s/%s (%s)", region, limitBytes, podNamespace, podName, podCgroup)
}

func (p *AMDGPUPlugin) rebuildCUAllocations(ctx context.Context, nodeName string) (map[string]cuallocation.Allocation, error) {
	selector := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	pods, err := utils.GetClient().CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list Pods: %w", err)
	}
	return p.buildCUAllocations(pods.Items)
}

// buildCUAllocations reconstructs device occupancy solely from durable Pod
// annotations. It fails closed if persisted allocations already overlap.
func (p *AMDGPUPlugin) buildCUAllocations(pods []corev1.Pod) (map[string]cuallocation.Allocation, error) {
	result := make(map[string]cuallocation.Allocation)
	owners := make(map[string][]string)

	for i := range pods {
		pod := &pods[i]
		if utils.IsPodInTerminatedState(pod) {
			continue
		}
		if strings.TrimSpace(pod.Annotations[utils.CuAllocation]) == "" {
			continue
		}

		podKey := pod.Namespace + "/" + pod.Name
		allocations, err := p.parseCuAllocation(pod.Annotations)
		if err != nil {
			return nil, fmt.Errorf("pod %s: %w", podKey, err)
		}
		for uuid, allocation := range allocations {
			totalCUs, err := p.getDeviceTotalCUs(uuid)
			if err != nil {
				return nil, fmt.Errorf("pod %s device %s: %w", podKey, uuid, err)
			}
			current, ok := result[uuid]
			if !ok {
				current, err = cuallocation.NewAllocation(totalCUs)
				if err != nil {
					return nil, fmt.Errorf("initialize device %s allocation: %w", uuid, err)
				}
				result[uuid] = current
				owners[uuid] = make([]string, totalCUs)
			}

			for cu := 0; cu < totalCUs; cu++ {
				word := cu / 64
				bit := uint(cu % 64)
				mask := uint64(1) << bit
				if allocation[word]&mask == 0 {
					continue
				}
				if current[word]&mask != 0 {
					return nil, fmt.Errorf("CU allocation overlap on device %s CU %d between Pods %s and %s", uuid, cu, owners[uuid][cu], podKey)
				}
			}

			updated, err := cuallocation.AddAllocation(current, totalCUs, allocation)
			if err != nil {
				return nil, fmt.Errorf("add Pod %s allocation for device %s: %w", podKey, uuid, err)
			}
			result[uuid] = updated
			for cu := 0; cu < totalCUs; cu++ {
				word := cu / 64
				bit := uint(cu % 64)
				if allocation[word]&(uint64(1)<<bit) != 0 {
					owners[uuid][cu] = podKey
				}
			}
		}
	}

	return result, nil
}

func (p *AMDGPUPlugin) validateNodeLockOwner(ctx context.Context, nodeName string, pod *corev1.Pod) error {
	node, err := utils.GetClient().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s before CU allocation commit: %w", nodeName, err)
	}
	lockValue, ok := node.Annotations[utils.NodeLockKey]
	if !ok {
		return fmt.Errorf("node %s lock is missing before CU allocation commit", nodeName)
	}
	_, namespace, name, err := utils.ParseNodeLock(lockValue)
	if err != nil {
		return fmt.Errorf("parse node %s lock: %w", nodeName, err)
	}
	if namespace != pod.Namespace || name != pod.Name {
		return fmt.Errorf("node %s lock is owned by %s/%s, not %s/%s", nodeName, namespace, name, pod.Namespace, pod.Name)
	}
	return nil
}

// isWholeGPU reports whether every device in the request takes its full GPU.
// The HAMi scheduler writes the full core and memory size for a whole-GPU
// request, while a fallback allocation leaves both at 0.
func (p *AMDGPUPlugin) isWholeGPU(devreq utils.ContainerDevices) bool {
	for _, d := range devreq {
		info := p.lookupDevice(d.UUID)
		if info == nil {
			return false
		}
		if (d.Usedcores > 0 && d.Usedcores < info.Devcore) || (d.Usedmem > 0 && d.Usedmem < info.Devmem) {
			return false
		}
	}
	return true
}

const kfdTopologyNodes = "/sys/class/kfd/kfd/topology/nodes"

var cpQueuesRe = regexp.MustCompile(`num_cp_queues\s(\d+)`)

// computeQueues returns the user compute queue (HQD) slots KFD reports for a
// GPU node. Processes sharing a GPU beyond these slots contend for dispatch.
var gfxVersionRe = regexp.MustCompile(`gfx_target_version\s(\d+)`)

// cuMaskUnit returns how many CUs HSA_CU_MASK must enable together on a GPU.
// RDNA (gfx10 and later) applies the mask per WGP, two CUs, and silently
// ignores a mask that enables only one CU of a pair; CDNA masks single CUs.
func (p *AMDGPUPlugin) cuMaskUnit(uuid, topoNodesDir string) int {
	data, err := p.deviceDataFromAllocationUUID(uuid, "")
	if err != nil {
		return 1
	}
	nodeId, _ := data["nodeId"].(int)
	v, err := amdgpu.ParseTopologyProperties(filepath.Join(topoNodesDir, strconv.Itoa(nodeId), "properties"), gfxVersionRe)
	if err == nil && v >= 100000 {
		return 2
	}
	return 1
}

func computeQueues(topoNodesDir string, nodeId int) (int64, bool) {
	q, err := amdgpu.ParseTopologyProperties(filepath.Join(topoNodesDir, strconv.Itoa(nodeId), "properties"), cpQueuesRe)
	return q, err == nil && q > 0
}

// lookupDevice finds the registered device for an AMD SMI UUID or a kubelet
// split id ("<bdf>#<slot>"), which only matches the registered pciBDF.
func (p *AMDGPUPlugin) lookupDevice(uuid string) *utils.DeviceInfo {
	bdf := strings.SplitN(uuid, "#", 2)[0]
	for _, d := range p.deviceCache {
		if d == nil {
			continue
		}
		if d.ID == uuid {
			return d
		}
		if pciBDF, ok := d.CustomInfo["pciBDF"].(string); ok && bdf != uuid && pciBDF == bdf {
			return d
		}
	}
	return nil
}

func (p *AMDGPUPlugin) getDeviceTotalCUs(uuid string) (int, error) {
	d := p.lookupDevice(uuid)
	if d == nil {
		return 0, fmt.Errorf("device %s not found in device cache", uuid)
	}
	if d.Devcore <= 0 {
		return 0, fmt.Errorf("invalid cu count for device %s: %d", uuid, d.Devcore)
	}
	return int(d.Devcore), nil
}

func (p *AMDGPUPlugin) hasPersistedCUAllocation(ctx context.Context, pod *corev1.Pod, expected string) (bool, error) {
	refreshed, err := utils.GetClient().CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return refreshed.Annotations[utils.CuAllocation] == expected, nil
}

// rocrVisibleList joins ROCr UUIDs for ROCR_VISIBLE_DEVICES. A GPU without
// one is named by its index among the container's GPUs, which ROCr enumerates
// in KFD node order.
// ponytail: render minor order stands in for KFD node order; read node ids if they ever diverge.
func rocrVisibleList(uuids []string, renderMinors []int) string {
	out := make([]string, len(uuids))
	for i, u := range uuids {
		if u != "" {
			out[i] = u
			continue
		}
		idx := 0
		for _, m := range renderMinors {
			if m < renderMinors[i] {
				idx++
			}
		}
		out[i] = strconv.Itoa(idx)
	}
	return strings.Join(out, ",")
}

// allocationToIDList converts a CU bitmap to ID_List grammar used by HSA_CU_MASK, e.g. "0-3,8,10-12".
func allocationToIDList(allocation cuallocation.Allocation, totalCUs int) string {
	if totalCUs <= 0 || len(allocation) == 0 {
		return "0"
	}
	ids := make([]int, 0)
	for i := 0; i < totalCUs; i++ {
		word := i / 64
		bit := uint(i % 64)
		if word >= len(allocation) {
			break
		}
		if (allocation[word] & (uint64(1) << bit)) != 0 {
			ids = append(ids, i)
		}
	}
	if len(ids) == 0 {
		return "0"
	}
	parts := make([]string, 0, len(ids))
	start := ids[0]
	prev := ids[0]
	flush := func(s, e int) {
		if s == e {
			parts = append(parts, fmt.Sprintf("%d", s))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s, e))
		}
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == prev+1 {
			prev = ids[i]
			continue
		}
		flush(start, prev)
		start = ids[i]
		prev = ids[i]
	}
	flush(start, prev)
	return strings.Join(parts, ",")
}

// idListToAllocation parses ID_List grammar (e.g. "0-3,8,10-12") into bitmap allocation.
func idListToAllocation(s string, totalCUs int) (cuallocation.Allocation, error) {
	allocation, err := cuallocation.NewAllocation(totalCUs)
	if err != nil {
		return nil, err
	}
	str := strings.TrimSpace(s)
	if str == "" {
		return allocation, nil
	}
	for _, part := range strings.Split(str, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.Split(part, "-")
			if len(bounds) != 2 {
				return nil, fmt.Errorf("invalid ID range %q", part)
			}
			start, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid range start %q: %w", part, err)
			}
			end, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid range end %q: %w", part, err)
			}
			if start < 0 || end < start || end >= totalCUs {
				return nil, fmt.Errorf("range out of bounds %q for totalCUs=%d", part, totalCUs)
			}
			for i := start; i <= end; i++ {
				word := i / 64
				bit := uint(i % 64)
				allocation[word] |= uint64(1) << bit
			}
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CU id %q: %w", part, err)
		}
		if id < 0 || id >= totalCUs {
			return nil, fmt.Errorf("CU id out of bounds %d for totalCUs=%d", id, totalCUs)
		}
		word := id / 64
		bit := uint(id % 64)
		allocation[word] |= uint64(1) << bit
	}
	return allocation, nil
}

// parseCuAllocation decodes utils.CuAllocation as JSON:
//
//	{ "<device-uuid>": "<ID_List>", ... }
//
// device-uuid must match DeviceInfo.ID and ID_List follows grammar like "0-3,8,10-12".
func (p *AMDGPUPlugin) parseCuAllocation(annotations map[string]string) (map[string]cuallocation.Allocation, error) {
	if annotations == nil {
		return nil, fmt.Errorf("nil annotations")
	}
	raw := strings.TrimSpace(annotations[utils.CuAllocation])
	if raw == "" {
		return nil, fmt.Errorf("empty %s", utils.CuAllocation)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse %s JSON: %w", utils.CuAllocation, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("no entries in %s", utils.CuAllocation)
	}
	out := make(map[string]cuallocation.Allocation, len(m))
	for uuid, idList := range m {
		if uuid == "" {
			return nil, fmt.Errorf("empty device uuid key in %s", utils.CuAllocation)
		}
		totalCUs, err := p.getDeviceTotalCUs(uuid)
		if err != nil {
			return nil, fmt.Errorf("device %q total CU lookup failed: %w", uuid, err)
		}
		alloc, err := idListToAllocation(idList, totalCUs)
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", uuid, err)
		}
		out[uuid] = alloc
	}
	return out, nil
}

// Lister serves as an interface between imlementation and Manager machinery. User passes
// implementation of this interface to NewManager function. Manager will use it to obtain resource
// namespace, monitor available resources and instantate a new plugin for them.
type AMDGPULister struct {
	ResUpdateChan chan dpm.PluginNameList
	Heartbeat     chan bool
	Signal        chan os.Signal
	// AllocatorPolicy is besteffort (default), binpack or spread.
	AllocatorPolicy string
	// SplitCount overrides how many workloads may share one GPU when > 0.
	SplitCount int
	// CDISpecDir enables CDI device injection with specs written there.
	CDISpecDir string
	// DmemBackend additionally caps sliced allocations through the kernel
	// dmem cgroup controller when available.
	DmemBackend bool
	// MuslFailClosed refuses a sliced allocation whose image is not
	// LD_AUDIT-compatible, instead of silently running it unprotected.
	MuslFailClosed bool
	// CtrPath and ContainerdSocket locate the ctr binary and containerd
	// socket this pod's own mount namespace can see, used only when
	// MuslFailClosed is set.
	CtrPath, ContainerdSocket string
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
	if l.SplitCount > 0 {
		splitCount = l.SplitCount
	}
	policy, err := allocator.NewPolicy(l.AllocatorPolicy)
	if err != nil {
		glog.Errorf("%v; using besteffort", err)
		policy = allocator.NewBestEffortPolicy()
	}
	options := []AMDGPUPluginOption{
		WithHeartbeat(l.Heartbeat),
		WithResource(resourceLastName),
		WithAllocator(policy),
		WithCDISpecDir(l.CDISpecDir),
		WithDmemBackend(l.DmemBackend),
		WithMuslFailClosed(l.MuslFailClosed, l.CtrPath, l.ContainerdSocket),
	}
	return NewAMDGPUPlugin(options...)
}
