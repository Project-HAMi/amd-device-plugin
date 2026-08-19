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
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/exporter"
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

// ModeAnnotation selects the allocation mode per pod: absent or "rocm" is
// soft CU masking, a compute partition type ("spx"/"cpx"/...) whole partitions.
const ModeAnnotation = "hami.io/amd-mode"

func IsPartitionMode(mode string) bool {
	switch mode {
	case "spx", "cpx", "dpx", "qpx":
		return true
	}
	return false
}

func stripPartitionModeSuffix(id string) string {
	if idx := strings.LastIndex(id, "#"); idx > 0 && IsPartitionMode(id[idx+1:]) {
		return id[:idx]
	}
	return id
}

// Plugin is identical to DevicePluginServer interface of device plugin API.
type AMDGPUPlugin struct {
	pluginapi.UnimplementedDevicePluginServer
	AMDGPUs map[string]map[string]interface{}
	// sysfsRoot overrides the /sys mount root used by device discovery; tests
	// point it at testdata, empty means /sys.
	sysfsRoot string
	// amdsmiUUIDLookup, amdsmiProductNameLookup and amdsmiMemoryPartitionLookup
	// are injectable for tests; nil means the AMD SMI cgo implementation.
	amdsmiUUIDLookup, amdsmiProductNameLookup, amdsmiMemoryPartitionLookup func([]string) (map[string]string, error)
	// amdsmiPartitionProfileLookup is injectable for tests; nil means the AMD
	// SMI cgo implementation.
	amdsmiPartitionProfileLookup func([]string) (map[string][]amdsmi.PartitionProfile, error)
	Heartbeat                    chan bool
	signal                       chan os.Signal
	disableWatchAndRegister      chan bool
	ackDisableWatchAndRegister   chan bool
	deviceCache                  []*utils.DeviceInfo
	Resource                     string
	devAllocator                 allocator.Policy
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
	// rocrUUIDToTopology maps ROCr-visible ids (GPU-<unique_id>, or the agent
	// index on APUs without a Device Serial Number) to topology keys.
	rocrUUIDToTopology map[string]string
	// sortedBDFs holds the registered PCI BDFs in list order (sorted topology
	// keys). Upstream HAMi invents flat "AMDGPU-<i>" device ids over node
	// capacity; device i resolves to GPU sortedBDFs[i/splitCount].
	sortedBDFs []string
	// partitionTemplates is the boot-time catalog of compute x NPS
	// combinations per card, with the NPS->physical memory lookup applied.
	// Held in memory only; never written to disk (immutable-OS friendly).
	partitionTemplates []amdgpu.DevicePartitionTemplates
	// bdfToROCrUUID maps a topology key to its ROCr UUID, for resolving
	// whole-GPU allocations and "<bdf>#<slot>" kubelet split ids during
	// Allocate.
	bdfToROCrUUID map[string]string
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

func WithSysfsRoot(root string) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.sysfsRoot = root
	}
}

// WithAmdSMI injects the AMD SMI lookups so registration runs without AMD SMI.
func WithAmdSMI(uuidLookup, productNameLookup, memoryPartitionLookup func([]string) (map[string]string, error)) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.amdsmiUUIDLookup = uuidLookup
		p.amdsmiProductNameLookup = productNameLookup
		p.amdsmiMemoryPartitionLookup = memoryPartitionLookup
	}
}

// WithAMDSPartitionProfiles injects the accelerator partition profile lookup
// so registration runs without AMD SMI.
func WithAMDSPartitionProfiles(lookup func([]string) (map[string][]amdsmi.PartitionProfile, error)) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.amdsmiPartitionProfileLookup = lookup
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
	err := p.devAllocator.Init(p.getDevices(), "")
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

	// Boot-time partition template catalog: per card, the valid compute x NPS
	// combinations and the NPS->physical-memory lookup. In-memory only;
	// regenerated on every boot.
	p.partitionTemplates, err = amdgpu.GeneratePartitionTemplates()
	if err != nil {
		glog.Warningf("partition template generation failed: %v", err)
	}
	for _, t := range p.partitionTemplates {
		glog.Infof("partition templates for %s (%s): current %s/%s physicalMemory=%d, %d combos",
			t.BDF, t.Product, t.CurrentComputePartition, t.CurrentMemoryPartition, t.CurrentPhysicalMemory, len(t.Templates))
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

// isSchedulableTopologyKey: a partitioned GPU parent has no capacity of its
// own and never registers.
func isSchedulableTopologyKey(key string, deviceData map[string]interface{}) bool {
	if !strings.HasPrefix(key, "amdgpu_xcp_") {
		computePartitionType, _ := deviceData["computePartitionType"].(string)
		return computePartitionType == ""
	}
	return true
}

// Registers the whole-partition form of an XCP entry; the soft entry stays
// for CU-masked rocm-mode pods.
func (p *AMDGPUPlugin) registerHardEntry(key string, deviceData map[string]interface{}, info *utils.DeviceInfo) bool {
	if !strings.HasPrefix(key, "amdgpu_xcp_") {
		return false
	}
	computePartitionType, ok := deviceData["computePartitionType"].(string)
	if !ok || computePartitionType == "" {
		return false
	}
	info.ID = info.ID + "#" + computePartitionType
	info.Mode = computePartitionType
	info.Count = 1
	return true
}

func (p *AMDGPUPlugin) getAPIDevices() []*utils.DeviceInfo {
	root := p.sysfsRoot
	if root == "" {
		root = "/sys"
	}
	uuidLookup, nameLookup, memoryPartitionLookup := p.amdsmiUUIDLookup, p.amdsmiProductNameLookup, p.amdsmiMemoryPartitionLookup
	if uuidLookup == nil {
		uuidLookup = amdsmi.GetAMDSMIUUIDs
	}
	if nameLookup == nil {
		nameLookup = amdsmi.GetAMDSMIProductNames
	}
	if memoryPartitionLookup == nil {
		memoryPartitionLookup = amdsmi.GetAMDSCurrentMemoryPartitions
	}
	partitionProfileLookup := p.amdsmiPartitionProfileLookup
	if partitionProfileLookup == nil {
		partitionProfileLookup = amdsmi.GetAMDGPUPartitionProfiles
	}
	p.AMDGPUs = amdgpu.GetAMDGPUs(root)

	// Whole-GPU capacity is read once per GPU through libdrm. XCP partitions
	// share their parent's PCI BDF and get an even share of that capacity.
	wholeCardByBDF := make(map[string]int)
	xcpCountByBDF := make(map[string]int)
	for key, deviceData := range p.AMDGPUs {
		bdf, _ := deviceData["devID"].(string)
		if bdf == "" {
			continue
		}
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			xcpCountByBDF[bdf]++
		} else {
			wholeCardByBDF[bdf], _ = deviceData["card"].(int)
		}
	}
	capacityByBDF := make(map[string]amdgpu.DeviceCapacity, len(wholeCardByBDF))
	for bdf, card := range wholeCardByBDF {
		if capacity, err := amdgpu.GetDeviceCapacity(fmt.Sprintf("card%d", card)); err != nil {
			glog.Warningf("libdrm capacity lookup failed for GPU %s (card%d): %v", bdf, card, err)
		} else {
			capacityByBDF[bdf] = capacity
		}
	}

	keys := make([]string, 0, len(p.AMDGPUs))
	for key := range p.AMDGPUs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Maps must exist even when empty so Allocate never dereferences nil.
	p.amdSMIUUIDToTopology = make(map[string]string, len(p.AMDGPUs))
	p.amdSMIUUIDToROCrUUID = make(map[string]string, len(p.AMDGPUs))
	p.rocrUUIDToTopology = make(map[string]string, len(p.AMDGPUs))
	p.sortedBDFs = make([]string, 0, len(p.AMDGPUs))
	p.bdfToROCrUUID = make(map[string]string, len(p.AMDGPUs))

	// Resolve a stable per-device UUID through the AMD SMI C API for whole
	// GPUs. XCP partitions share the parent PCI BDF, so the BDF-keyed AMD SMI
	// lookup would collide; their IDs are ROCr UUIDs (unique per KFD node).
	bdfs := make([]string, 0, len(p.AMDGPUs))
	for _, deviceData := range p.AMDGPUs {
		if bdf, ok := deviceData["devID"].(string); ok {
			bdfs = append(bdfs, bdf)
		}
	}
	amdSMIUUIDs, err := uuidLookup(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI UUID lookup incomplete; GPUs without an AMD SMI UUID will not be registered: %v", err)
	}
	rocrUUIDs := amdgpu.GetROCrUUIDsFromTopology(filepath.Join(root, "class/kfd/kfd"))
	rocrIndexes := amdgpu.GetROCrIndexesFromTopology(filepath.Join(root, "class/kfd/kfd"))
	amdSMIProductNames, err := nameLookup(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI product-name lookup incomplete; using amd-gpu where necessary: %v", err)
	}
	memoryPartitions, err := memoryPartitionLookup(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI memory-partition lookup incomplete; keeping sysfs values: %v", err)
	}
	partitionProfiles, err := partitionProfileLookup(bdfs)
	if err != nil {
		glog.Warningf("AMD SMI partition-profile lookup incomplete: %v", err)
	}

	out := make([]*utils.DeviceInfo, 0, len(keys)*2)
	for _, key := range keys {
		deviceData := p.AMDGPUs[key]
		if !isSchedulableTopologyKey(key, deviceData) {
			glog.Infof("skip topology entry %s (no allocatable capacity)", key)
			continue
		}

		card, _ := deviceData["card"].(int)
		numa, _ := deviceData["numaNode"].(int)
		bdf, ok := deviceData["devID"].(string)
		if !ok || bdf == "" {
			glog.Errorf("skip GPU %s: missing PCI BDF in topology", key)
			continue
		}
		capacity := capacityByBDF[bdf]
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			// XCP partitions advertise an even share of the whole GPU's
			// VRAM and CU count, not the full-GPU capacity.
			capacity = amdgpu.PartitionCapacity(capacity, xcpCountByBDF[bdf])
		}
		renderD, ok := deviceData["renderD"].(int)
		if !ok || renderD <= 0 {
			glog.Errorf("skip GPU %s: missing or invalid render node in topology", key)
			continue
		}
		rocrUUID, found := rocrUUIDs[renderD]
		if !found || rocrUUID == "" {
			// APUs without a PCI Device Serial Number report unique_id 0 and
			// ROCr addresses them by agent index instead of a GPU- UUID.
			if idx, ok := rocrIndexes[renderD]; ok {
				rocrUUID = strconv.Itoa(idx)
			} else {
				glog.Errorf("skip GPU %s: no ROCr UUID found for renderD%d", key, renderD)
				continue
			}
		}
		p.rocrUUIDToTopology[rocrUUID] = key
		p.bdfToROCrUUID[key] = rocrUUID
		p.sortedBDFs = append(p.sortedBDFs, key)

		// Soft entry ID: AMD SMI UUID for whole GPUs, ROCr UUID for partitions.
		infoID := rocrUUID
		if !strings.HasPrefix(key, "amdgpu_xcp_") {
			uuid, found := amdSMIUUIDs[strings.ToLower(bdf)]
			if !found || uuid == "" {
				// Do not fall back to a node/BDF-derived ID. The scheduler must
				// only see stable, hardware-bound AMD SMI UUIDs.
				glog.Errorf("skip GPU %s: AMD SMI did not return a UUID for topology BDF %s", key, bdf)
				continue
			}
			p.amdSMIUUIDToTopology[uuid] = key
			p.amdSMIUUIDToROCrUUID[uuid] = rocrUUID
			infoID = uuid
			// AMD SMI reports the current memory partition for whole GPUs;
			// prefer it over the sysfs read used for XCP fallback.
			if partition, ok := memoryPartitions[strings.ToLower(bdf)]; ok && partition != "" {
				deviceData["memoryPartitionType"] = partition
			}
		}

		customInfo := map[string]any{"pciBDF": strings.ToLower(bdf)}
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			memoryPartitionType, _ := deviceData["memoryPartitionType"].(string)
			computePartitionType, _ := deviceData["computePartitionType"].(string)
			customInfo["partitionProfile"] = computePartitionType + "_" + memoryPartitionType
		} else if profiles, ok := partitionProfiles[strings.ToLower(bdf)]; ok {
			// Whole GPUs advertise the modes they can be set to; XCP entries
			// advertise the mode they are currently partitioned into.
			customInfo["partitionProfiles"] = profiles
		}
		nodeId, _ := deviceData["nodeId"].(int)
		if q, ok := computeQueues(filepath.Join(root, "class/kfd/kfd/topology/nodes"), nodeId); ok {
			customInfo["computeQueues"] = q
		}
		deviceType := "amd-gpu"
		if productName, ok := amdSMIProductNames[strings.ToLower(bdf)]; ok && productName != "" {
			deviceType = productName
		}

		soft := &utils.DeviceInfo{
			ID:           infoID,
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
		}
		out = append(out, soft)
		if p.registerHardEntry(key, deviceData, soft) {
			glog.Infof("registered hard partition device %s (mode %s) alongside soft device %s", soft.ID, soft.Mode, infoID)
		}
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

func kubeletDeviceID(key string, splitIdx int) string {
	return fmt.Sprintf("%s#%d", key, splitIdx)
}

func hardKubeletDeviceID(key string, deviceData map[string]interface{}) string {
	if computePartitionType, _ := deviceData["computePartitionType"].(string); computePartitionType != "" {
		return fmt.Sprintf("%s#%s", key, computePartitionType)
	}
	return ""
}

func (p *AMDGPUPlugin) getDevices() []*allocator.Device {
	devices := amdgpu.GetAMDGPUs()
	var deviceList []*allocator.Device

	for id, deviceData := range devices {
		if !isSchedulableTopologyKey(id, deviceData) {
			continue
		}
		for splitIdx := 0; splitIdx < splitCount; splitIdx++ {
			device := &allocator.Device{
				Id:                   kubeletDeviceID(id, splitIdx),
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
		if hardID := hardKubeletDeviceID(id, deviceData); hardID != "" {
			deviceList = append(deviceList, &allocator.Device{
				Id:                   hardID,
				Card:                 deviceData["card"].(int),
				RenderD:              deviceData["renderD"].(int),
				DevId:                deviceData["devID"].(string),
				ComputePartitionType: deviceData["computePartitionType"].(string),
				MemoryPartitionType:  deviceData["memoryPartitionType"].(string),
				NodeId:               deviceData["nodeId"].(int),
				NumaNode:             deviceData["numaNode"].(int),
			})
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

// kubeletDevicesFor publishes defaultSplitCount soft slice slots plus one
// whole-partition device for XCP entries; the pod annotation picks per pod.
func (p *AMDGPUPlugin) kubeletDevicesFor(id string, deviceData map[string]interface{}) []*pluginapi.Device {
	numas := []int64{int64(deviceData["numaNode"].(int))}
	glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

	numaNodes := make([]*pluginapi.NUMANode, len(numas))
	for j, v := range numas {
		numaNodes[j] = &pluginapi.NUMANode{
			ID: int64(v),
		}
	}

	devs := make([]*pluginapi.Device, 0, defaultSplitCount+1)
	for splitIdx := 0; splitIdx < defaultSplitCount; splitIdx++ {
		devs = append(devs, &pluginapi.Device{
			ID:     kubeletDeviceID(id, splitIdx),
			Health: pluginapi.Healthy,
			Topology: &pluginapi.TopologyInfo{
				Nodes: numaNodes,
			},
		})
	}
	if hardID := hardKubeletDeviceID(id, deviceData); hardID != "" {
		devs = append(devs, &pluginapi.Device{
			ID:     hardID,
			Health: pluginapi.Healthy,
			Topology: &pluginapi.TopologyInfo{
				Nodes: numaNodes,
			},
		})
	}
	return devs
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

	devs := make([]*pluginapi.Device, 0, len(p.AMDGPUs)*(splitCount+1))
	isHomogeneous := amdgpu.IsHomogeneous()
	// Initialize a map to store partitionType based device list
	resourceTypeDevs := make(map[string][]*pluginapi.Device)

	if isHomogeneous {
		// limit scope for hwloc
		func() {
			for id, device := range p.AMDGPUs {
				if !isSchedulableTopologyKey(id, device) {
					continue
				}
				devs = append(devs, p.kubeletDevicesFor(id, device)...)
			}
		}()
		glog.Infof("ListAndWatch: sending %d split devices (homogeneous)", len(devs))
		s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	} else {
		func() {
			for id, device := range p.AMDGPUs {
				if !isSchedulableTopologyKey(id, device) {
					continue
				}
				partitionType := device["computePartitionType"].(string) + "_" + device["memoryPartitionType"].(string)
				for _, dev := range p.kubeletDevicesFor(id, device) {
					resourceTypeDevs[partitionType] = append(resourceTypeDevs[partitionType], dev)
				}
			}
		}()
		// Send the appropriate list of devices based on the partitionType
		if devList, exists := resourceTypeDevs[p.Resource]; exists {
			glog.Infof("ListAndWatch: sending %d split devices for resource %s", len(devList), p.Resource)
			s.Send(&pluginapi.ListAndWatchResponse{Devices: devList})
		} else {
			glog.Warningf("ListAndWatch: no devices for resource %s; partition types: %v", p.Resource, resourceTypeDevs)
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
		glog.Infof("GetPreferredAllocation: available=%v must=%v size=%d", req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, req.AllocationSize)
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
// parseAMDGPUIndex extracts the flat counter from upstream HAMi's
// "<node>-AMDGPU-<i>" device ids, or -1 for any other id spelling.
func parseAMDGPUIndex(id string) int {
	idx := strings.LastIndex(id, "AMDGPU-")
	if idx < 0 {
		return -1
	}
	n, err := strconv.Atoi(id[idx+len("AMDGPU-"):])
	if err != nil {
		return -1
	}
	return n
}

func (p *AMDGPUPlugin) deviceDataFromAllocationUUID(uuid, _ string) (map[string]interface{}, error) {
	uuid = stripPartitionModeSuffix(uuid)
	if topoKey, ok := p.amdSMIUUIDToTopology[uuid]; ok {
		if d, found := p.AMDGPUs[topoKey]; found {
			return d, nil
		}
		return nil, fmt.Errorf("AMD SMI UUID %q resolves to unavailable topology key %q", uuid, topoKey)
	}
	if topoKey, ok := p.rocrUUIDToTopology[uuid]; ok {
		if d, found := p.AMDGPUs[topoKey]; found {
			return d, nil
		}
		return nil, fmt.Errorf("ROCr UUID %q resolves to unavailable topology key %q", uuid, topoKey)
	}
	// kubelet split ids are "<bdf>#<slot>"; resolve the bare BDF directly.
	if bdf := strings.SplitN(uuid, "#", 2)[0]; bdf != uuid {
		if d, found := p.AMDGPUs[bdf]; found {
			return d, nil
		}
	}
	if i := parseAMDGPUIndex(uuid); i >= 0 {
		// Upstream HAMi device i is the i-th split slot in registration order.
		gpuIdx := i / splitCount
		if gpuIdx < len(p.sortedBDFs) {
			if d, found := p.AMDGPUs[p.sortedBDFs[gpuIdx]]; found {
				return d, nil
			}
		}
	}
	return nil, fmt.Errorf("no local GPU topology entry for device id %q", uuid)
}

func (p *AMDGPUPlugin) rocrUUIDFromAllocationUUID(uuid string) (string, error) {
	// XCP partitions publish ROCr UUIDs as DeviceInfo.ID, optionally with a
	// "#<partition-type>" hard-entry suffix.
	uuid = stripPartitionModeSuffix(uuid)
	if strings.HasPrefix(uuid, "GPU-") {
		return uuid, nil
	}
	if rocrUUID, ok := p.amdSMIUUIDToROCrUUID[uuid]; ok && rocrUUID != "" {
		return rocrUUID, nil
	}
	if bdf := strings.SplitN(uuid, "#", 2)[0]; bdf != uuid {
		if rocrUUID, ok := p.bdfToROCrUUID[bdf]; ok && rocrUUID != "" {
			return rocrUUID, nil
		}
	}
	if i := parseAMDGPUIndex(uuid); i >= 0 {
		gpuIdx := i / splitCount
		if gpuIdx < len(p.sortedBDFs) {
			if rocrUUID, ok := p.bdfToROCrUUID[p.sortedBDFs[gpuIdx]]; ok && rocrUUID != "" {
				return rocrUUID, nil
			}
		}
	}
	return "", fmt.Errorf("no ROCr UUID for device id %q", uuid)
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
				_, deltaAllocation, err := cuallocation.AllocateN(baseAllocation, totalCUs, cores)
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
			car.Envs["ROCR_VISIBLE_DEVICES"] = strings.Join(rocrVisibleDevices, ",")
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
				car.Envs["HIP_DEVICE_MEMORY_LIMIT"] = fmt.Sprintf("%vm", devreq[0].Usedmem)
				car.Envs["LD_AUDIT"] = "/usr/local/vgpu/libamvgpu.so"
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

var cpQueuesRe = regexp.MustCompile(`num_cp_queues\s(\d+)`)

// computeQueues returns the user compute queue (HQD) slots KFD reports for a
// GPU node. Processes sharing a GPU beyond these slots contend for dispatch.
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
	}
	return NewAMDGPUPlugin(options...)
}
