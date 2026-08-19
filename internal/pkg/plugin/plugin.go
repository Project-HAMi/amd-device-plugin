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

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/allocator"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/cuallocation"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/exporter"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const defaultSplitCount = 10

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
	AMDGPUs map[string]map[string]interface{}
	// sysfsRoot overrides the /sys mount root used by device discovery; tests
	// point it at testdata, empty means /sys.
	sysfsRoot string
	// amdsmiUUIDLookup, amdsmiProductNameLookup and amdsmiMemoryPartitionLookup
	// are injectable for tests; nil means the AMD SMI cgo implementation.
	amdsmiUUIDLookup, amdsmiProductNameLookup, amdsmiMemoryPartitionLookup func([]string) (map[string]string, error)
	// amdsmiPartitionProfileLookup is injectable for tests; nil means the AMD
	// SMI cgo implementation.
	amdsmiPartitionProfileLookup func([]string) (map[string][]amdgpu.PartitionProfile, error)
	Heartbeat                    chan bool
	signal                       chan os.Signal
	disableWatchAndRegister      chan bool
	ackDisableWatchAndRegister   chan bool
	deviceCache                  []*utils.DeviceInfo
	Resource                     string
	devAllocator                 allocator.Policy
	allocatorInitError           bool
	// operatingMode is "cu" (soft whole-GPU + CU slices, default) or
	// "partition" (hard compute partitions only). Resolved in Start() from
	// the OPERATING_MODE env, overridable per node via the
	// hami.io/amd-operating-mode annotation.
	operatingMode string
	// computePartition is the target compute partition type (spx/dpx/qpx/cpx)
	// the plugin applies through the AMD SMI API at startup; empty leaves the
	// hardware mode untouched. Only applied in partition mode. Resolved like
	// operatingMode (COMPUTE_PARTITION env, hami.io/amd-compute-partition
	// annotation).
	computePartition string
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
	// capacity; device i resolves to GPU sortedBDFs[i/defaultSplitCount].
	sortedBDFs []string
	// partitionTemplates is the boot-time catalog of compute x NPS
	// combinations per card, with the NPS->physical memory lookup applied.
	// Held in memory only; never written to disk (immutable-OS friendly).
	partitionTemplates []amdgpu.DevicePartitionTemplates
	// bdfToROCrUUID maps a topology key to its ROCr UUID, for resolving
	// whole-GPU allocations from kubelet device ids.
	bdfToROCrUUID map[string]string
}

type AMDGPUPluginOption func(*AMDGPUPlugin)

func NewAMDGPUPlugin(options ...AMDGPUPluginOption) *AMDGPUPlugin {
	amdGpuPlugin := &AMDGPUPlugin{operatingMode: "cu"}
	if mode := os.Getenv("OPERATING_MODE"); mode != "" {
		amdGpuPlugin.operatingMode = mode
	}
	if partition := os.Getenv("COMPUTE_PARTITION"); partition != "" {
		amdGpuPlugin.computePartition = partition
	}
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
func WithAMDSPartitionProfiles(lookup func([]string) (map[string][]amdgpu.PartitionProfile, error)) AMDGPUPluginOption {
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
	// Per-node operating-mode override; must resolve before allocator and
	// registration use it.
	if node, err := utils.GetNode(os.Getenv(utils.NodeNameEnvName)); err == nil {
		if mode, ok := node.Annotations[operatingModeAnnotation]; ok && mode != "" {
			p.operatingMode = mode
		}
		if partition, ok := node.Annotations[computePartitionAnnotation]; ok && partition != "" {
			p.computePartition = partition
		}
	} else {
		glog.Warningf("operating-mode: cannot read node annotations, keeping %q: %v", p.operatingMode, err)
	}
	glog.Infof("operating mode: %s, compute partition: %s", p.operatingMode, p.computePartition)

	// In partition mode with a target compute partition set, flip the GPUs
	// through the AMD SMI API before any registration reads the current
	// hardware mode. Busy GPUs fail and keep their current mode; the rest
	// still flip.
	if p.operatingMode == "partition" && p.computePartition != "" {
		var bdfs []string
		for _, deviceData := range amdgpu.GetAMDGPUs() {
			if bdf, ok := deviceData["devID"].(string); ok && bdf != "" {
				bdfs = append(bdfs, bdf)
			}
		}
		flipStart := time.Now()
		if err := amdgpu.SetGPUComputePartitions(bdfs, p.computePartition); err != nil {
			glog.Warningf("compute-partition flip to %s incomplete: %v", p.computePartition, err)
		}
		glog.V(2).Infof("compute-partition flip to %s took %s", p.computePartition, time.Since(flipStart))
	}

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
	// operatingModeAnnotation overrides the OPERATING_MODE env per node:
	// "cu" or "partition". NVIDIA's MIG config uses the same per-node
	// annotation pattern (nvidia.com/mig.config).
	operatingModeAnnotation = "hami.io/amd-operating-mode"
	// computePartitionAnnotation is the target compute partition type
	// (spx/dpx/qpx/cpx) the plugin applies via the AMD SMI API at startup.
	// Overrides the COMPUTE_PARTITION env per node.
	computePartitionAnnotation = "hami.io/amd-compute-partition"
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

// isSchedulableTopologyKey: in cu mode every whole-GPU key registers (CU
// slicing over the full GPU); XCP partition keys only register in partition
// mode. In partition mode a whole-GPU key registers only when the GPU has at
// most one partition (spx): a QPX/DPX whole GPU is replaced by its XCP
// partitions, and on kernels where KFD does not expose XCP nodes the whole
// GPU registers as a hard single-partition device with its capacity unshared.
func (p *AMDGPUPlugin) isSchedulableTopologyKey(key string, deviceData map[string]interface{}, partitionsByBDF map[string]int) bool {
	if strings.HasPrefix(key, "amdgpu_xcp_") {
		return p.operatingMode == "partition"
	}
	if p.operatingMode == "partition" {
		bdf, _ := deviceData["devID"].(string)
		return partitionsByBDF[bdf] <= 1
	}
	return true
}

// numPartitionsForMode returns how many compute partitions the current
// compute type creates, from the AMD SMI profile table; 1 when unknown.
// This is the correct capacity divisor (amdgpu_xcp_ child counts are wrong:
// gfx950 exposes 7 XCD chiplets per GPU regardless of partition mode).
func numPartitionsForMode(profiles []amdgpu.PartitionProfile, computeType string) int {
	for _, profile := range profiles {
		if strings.EqualFold(profile.Type, computeType) {
			return profile.NumPartitions
		}
	}
	return 1
}

// Registers the whole-partition form of a device. In partition mode every
// schedulable device (whole GPU with a compute type, or XCP partition) gets
// a hard "id#mode" entry; in cu mode only XCP partitions do.
func (p *AMDGPUPlugin) registerHardEntry(key string, deviceData map[string]interface{}, info *utils.DeviceInfo) bool {
	if p.operatingMode != "partition" && !strings.HasPrefix(key, "amdgpu_xcp_") {
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
		uuidLookup = amdgpu.GetAMDSMIUUIDs
	}
	if nameLookup == nil {
		nameLookup = amdgpu.GetAMDSMIProductNames
	}
	if memoryPartitionLookup == nil {
		memoryPartitionLookup = amdgpu.GetAMDSCurrentMemoryPartitions
	}
	partitionProfileLookup := p.amdsmiPartitionProfileLookup
	if partitionProfileLookup == nil {
		partitionProfileLookup = amdgpu.GetAMDGPUPartitionProfiles
	}
	p.AMDGPUs = amdgpu.GetAMDGPUs(root)

	// Whole-GPU capacity is read once per GPU through libdrm. XCP partitions
	// share their parent's PCI BDF and get an even share of that capacity.
	wholeCardByBDF := make(map[string]int)
	for key, deviceData := range p.AMDGPUs {
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			continue
		}
		if bdf, ok := deviceData["devID"].(string); ok && bdf != "" {
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
	glog.Infof("topology keys registered: %v", keys)

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

	// Per-GPU partition count of the current compute type; the capacity
	// divisor for XCP entries and the schedulability gate for whole GPUs.
	partitionsByBDF := make(map[string]int, len(p.AMDGPUs))
	for key, deviceData := range p.AMDGPUs {
		if strings.HasPrefix(key, "amdgpu_xcp_") {
			continue
		}
		bdf, _ := deviceData["devID"].(string)
		if bdf == "" {
			continue
		}
		computeType, _ := deviceData["computePartitionType"].(string)
		if profiles, ok := partitionProfiles[strings.ToLower(bdf)]; ok {
			partitionsByBDF[bdf] = numPartitionsForMode(profiles, computeType)
		}
	}

	out := make([]*utils.DeviceInfo, 0, len(keys)*2)
	for _, key := range keys {
		deviceData := p.AMDGPUs[key]
		if !p.isSchedulableTopologyKey(key, deviceData, partitionsByBDF) {
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
			capacity = amdgpu.PartitionCapacity(capacity, partitionsByBDF[bdf])
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
		deviceType := "amd-gpu"
		if productName, ok := amdSMIProductNames[strings.ToLower(bdf)]; ok && productName != "" {
			deviceType = productName
		}

		soft := &utils.DeviceInfo{
			ID:           infoID,
			Index:        uint(card),
			Count:        defaultSplitCount,
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
	// partitionsByBDF gates whole GPUs in partition mode; the profile lookup
	// is not injectable here, so a GPU without profiles stays schedulable
	// (spx default) instead of disappearing.
	partitionsByBDF := make(map[string]int)

	for id, deviceData := range devices {
		if !p.isSchedulableTopologyKey(id, deviceData, partitionsByBDF) {
			continue
		}
		if p.operatingMode != "partition" {
			for splitIdx := 0; splitIdx < defaultSplitCount; splitIdx++ {
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
	if p.operatingMode != "partition" {
		for splitIdx := 0; splitIdx < defaultSplitCount; splitIdx++ {
			devs = append(devs, &pluginapi.Device{
				ID:     kubeletDeviceID(id, splitIdx),
				Health: pluginapi.Healthy,
				Topology: &pluginapi.TopologyInfo{
					Nodes: numaNodes,
				},
			})
		}
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

	devs := make([]*pluginapi.Device, 0, len(p.AMDGPUs)*(defaultSplitCount+1))
	var isHomogeneous bool
	isHomogeneous = amdgpu.IsHomogeneous()
	// Initialize a map to store partitionType based device list
	resourceTypeDevs := make(map[string][]*pluginapi.Device)

	if isHomogeneous {
		// limit scope for hwloc
		func() {
			// partitionsByBDF gates whole GPUs in partition mode. The device
			// map here has no partition-profile lookup; leave empty so whole
			// GPUs stay schedulable (spx default) instead of disappearing.
			for id, device := range p.AMDGPUs {
				if !p.isSchedulableTopologyKey(id, device, map[string]int{}) {
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
				if !p.isSchedulableTopologyKey(id, device, map[string]int{}) {
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
			// Mixed node under the single strategy: the resource name is
			// "gpu", not a partition type, so publish every device.
			devList := make([]*pluginapi.Device, 0)
			for _, devs := range resourceTypeDevs {
				devList = append(devList, devs...)
			}
			glog.Infof("ListAndWatch: sending %d split devices for resource %s (mixed node)", len(devList), p.Resource)
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
		glog.Infof("GetPreferredAllocation: available=%v must=%v size=%d", req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, req.AllocationSize)
		allocated_ids, err := p.devAllocator.Allocate(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
		if err != nil {
			// kubelet passes its checkpoint-persisted ids (bare BDFs from the
			// pre-fork plugin), which the split-keyed allocator cannot map.
			// GPA is advisory; Allocate resolves the pod annotation, so echo
			// kubelet's own ids instead of failing admission.
			glog.Warningf("preferred allocation failed (%v); echoing kubelet device ids", err)
			allocated_ids = req.MustIncludeDeviceIDs
			for _, id := range req.AvailableDeviceIDs {
				if len(allocated_ids) >= int(req.AllocationSize) {
					break
				}
				allocated_ids = append(allocated_ids, id)
			}
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
	if i := parseAMDGPUIndex(uuid); i >= 0 {
		// Upstream HAMi device i is the i-th split slot in registration order.
		gpuIdx := i / defaultSplitCount
		glog.Infof("resolving upstream AMDGPU id %s: index=%d gpuIdx=%d splitCount=%d sortedBDFs=%d", uuid, i, gpuIdx, defaultSplitCount, len(p.sortedBDFs))
		if gpuIdx < len(p.sortedBDFs) {
			if d, found := p.AMDGPUs[p.sortedBDFs[gpuIdx]]; found {
				return d, nil
			}
			glog.Infof("resolved gpuIdx %d -> %s but missing from AMDGPUs", gpuIdx, p.sortedBDFs[gpuIdx])
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
	if i := parseAMDGPUIndex(uuid); i >= 0 {
		gpuIdx := i / defaultSplitCount
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

	glog.Infof("Allocate request: %+v", r)
	nodename := os.Getenv(utils.NodeNameEnvName)
	hostHookPath := os.Getenv("HOST_HOOK_PATH")

	// Whole-GPU allocations (upstream HAMi 2.9, Usedcores=0) carry no cores, so
	// kubelet device ids are resolved when the annotation path is unavailable.
	current, err := utils.GetPendingPod(ctx, nodename)
	if err != nil {
		glog.Warningf("no pending pod on %s, falling back to kubelet device ids: %v", nodename, err)
	}
	var podDevices string
	var podCuAllocList map[string]string
	var cuAllocationSnapshot map[string]cuallocation.Allocation
	if current != nil {
		podDevices = current.Annotations[utils.DeviceAllocation]
		podCuAllocList = map[string]string{}
		if raw := strings.TrimSpace(current.Annotations[utils.CuAllocation]); raw != "" {
			if err := json.Unmarshal([]byte(raw), &podCuAllocList); err != nil {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, fmt.Errorf("parse existing %s: %w", utils.CuAllocation, err)
			}
		}
		// Pod annotations are the source of truth: the scheduler holds the node
		// lock until the allocation persists, so a direct read beats informer delivery.
		cuAllocationSnapshot, err = p.rebuildCUAllocations(ctx, nodename)
		if err != nil {
			utils.PodAllocationFailed(nodename, current, NodeLockName)
			return &pluginapi.AllocateResponse{}, fmt.Errorf("rebuild CU allocation for node %s: %w", nodename, err)
		}
		glog.Infof("Allocate pod name is %s/%s, annotation is %+v", current.Namespace, current.Name, current.Annotations)
	}

	for idx := range r.ContainerRequests {
		car := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{},
		}
		rocrVisibleDevices := make([]string, 0, len(r.ContainerRequests[idx].DevicesIDs))

		car.Devices = append(car.Devices, &pluginapi.DeviceSpec{
			HostPath:      "/dev/kfd",
			ContainerPath: "/dev/kfd",
			Permissions:   "rw",
		})
		appendDeviceNodes := func(deviceData map[string]interface{}) error {
			cardMinor, cok := deviceData["card"].(int)
			renderMinor, rok := deviceData["renderD"].(int)
			if !cok || !rok {
				return fmt.Errorf("invalid card/renderD in topology")
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
			return nil
		}

		var devreq utils.ContainerDevices
		currentCtrName := ""
		fromAnnotation := false
		if current != nil {
			if ctr, d, rerr := utils.GetNextDeviceRequest("amd", *current); rerr == nil && len(d) == len(r.ContainerRequests[idx].DevicesIDs) {
				currentCtrName, devreq, fromAnnotation = ctr.Name, d, true
			}
		}
		glog.Infof("deviceAllocateFromAnnotation(container=%s)=%+v fromAnnotation=%v", currentCtrName, devreq, fromAnnotation)

		if fromAnnotation {
			for _, d := range devreq {
				glog.Infof("Allocating device from annotation UUID: %s", d.UUID)
				deviceData, topoErr := p.deviceDataFromAllocationUUID(d.UUID, nodename)
				if topoErr != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, topoErr
				}
				if nerr := appendDeviceNodes(deviceData); nerr != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, fmt.Errorf("topology for UUID %q: %w", d.UUID, nerr)
				}
				rocrUUID, rocrErr := p.rocrUUIDFromAllocationUUID(d.UUID)
				if rocrErr != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, rocrErr
				}
				rocrVisibleDevices = append(rocrVisibleDevices, rocrUUID)
			}
		} else {
			// Whole-GPU fallback: resolve kubelet's own device ids ("BDF#slot").
			for _, id := range r.ContainerRequests[idx].DevicesIDs {
				bdf := strings.SplitN(id, "#", 2)[0]
				deviceData, found := p.AMDGPUs[bdf]
				if !found {
					return &pluginapi.AllocateResponse{}, fmt.Errorf("no topology entry for kubelet device id %q", id)
				}
				if nerr := appendDeviceNodes(deviceData); nerr != nil {
					return &pluginapi.AllocateResponse{}, fmt.Errorf("topology for kubelet device id %q: %w", id, nerr)
				}
				rocrUUID, ok := p.bdfToROCrUUID[bdf]
				if !ok || rocrUUID == "" {
					return &pluginapi.AllocateResponse{}, fmt.Errorf("no ROCr UUID for kubelet device id %q", id)
				}
				rocrVisibleDevices = append(rocrVisibleDevices, rocrUUID)
			}
		}
		car.Envs["ROCR_VISIBLE_DEVICES"] = strings.Join(rocrVisibleDevices, ",")

		// CU slicing applies only when the scheduler handed out cores. Upstream
		// HAMi's AMD driver always requests whole GPUs (Usedcores=0), so the CU
		// mask, memory limit and LD_AUDIT hook are skipped there.
		if fromAnnotation && len(devreq) > 0 && devreq[0].Usedcores > 0 {
			err = utils.EraseNextDeviceTypeFromAnnotation("amd", *current)
			if err != nil {
				utils.PodAllocationFailed(nodename, current, NodeLockName)
				return &pluginapi.AllocateResponse{}, err
			}

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
				_, deltaAllocation, err := cuallocation.AllocateN(baseAllocation, totalCUs, int(d.Usedcores))
				if err != nil {
					utils.PodAllocationFailed(nodename, current, NodeLockName)
					return &pluginapi.AllocateResponse{}, fmt.Errorf("allocate cu for %s: %w", d.UUID, err)
				}
				cuList := allocationToIDList(deltaAllocation, totalCUs)
				// HSA_CU_MASK: GPU_list:CU_list[;GPU_list:CU_list]*.
				// Use container-local device index as GPU_list and ID_List as CU_list.
				hsaCuSets = append(hsaCuSets, fmt.Sprintf("%d:%s", i, cuList))

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
			car.Envs["HIP_DEVICE_MEMORY_LIMIT"] = fmt.Sprintf("%vm", devreq[0].Usedmem)
			car.Envs["LD_AUDIT"] = "/usr/local/vgpu/libamvgpu.so"
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

	if len(podCuAllocList) > 0 {
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

	if current != nil {
		utils.PodAllocationTrySuccess(nodename, podDevices, NodeLockName, current)
	}
	glog.Infoln("Allocate Response", response.ContainerResponses)

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
			return nil, fmt.Errorf("Pod %s: %w", podKey, err)
		}
		for uuid, allocation := range allocations {
			totalCUs, err := p.getDeviceTotalCUs(uuid)
			if err != nil {
				return nil, fmt.Errorf("Pod %s device %s: %w", podKey, uuid, err)
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

func (p *AMDGPUPlugin) getDeviceTotalCUs(uuid string) (int, error) {
	for _, d := range p.deviceCache {
		if d == nil || d.ID != uuid {
			continue
		}
		if d.Devcore <= 0 {
			return 0, fmt.Errorf("invalid cu count for device %s: %d", uuid, d.Devcore)
		}
		return int(d.Devcore), nil
	}
	return 0, fmt.Errorf("device %s not found in device cache", uuid)
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
