package amdgpu

/*
#cgo CFLAGS: -I/opt/rocm/include
#cgo LDFLAGS: -L/opt/rocm/lib -lamd_smi -lstdc++
#include <amd_smi/amdsmi.h>
#include <stdio.h>
#include <stdlib.h>

static amdsmi_status_t amdsmi_uuid_for_bdf(const char *bdf_text, char *uuid,
                                           unsigned int *uuid_length) {
	unsigned long long domain;
	unsigned int bus, device, function;
	char trailing;
	if (sscanf(bdf_text, "%llx:%x:%x.%x%c", &domain, &bus, &device, &function,
	           &trailing) != 4 || bus > 0xff || device > 0x1f || function > 7) {
		return AMDSMI_STATUS_INVAL;
	}

	amdsmi_bdf_t bdf = {0};
	bdf.bdf.domain_number = domain;
	bdf.bdf.bus_number = bus;
	bdf.bdf.device_number = device;
	bdf.bdf.function_number = function;
	amdsmi_processor_handle processor = NULL;
	amdsmi_status_t status = amdsmi_get_processor_handle_from_bdf(bdf, &processor);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	return amdsmi_get_gpu_device_uuid(processor, uuid_length, uuid);
}

static amdsmi_status_t amdsmi_product_name_for_bdf(const char *bdf_text,
                                                    char *market_name) {
	unsigned long long domain;
	unsigned int bus, device, function;
	char trailing;
	if (sscanf(bdf_text, "%llx:%x:%x.%x%c", &domain, &bus, &device, &function,
	           &trailing) != 4 || bus > 0xff || device > 0x1f || function > 7) {
		return AMDSMI_STATUS_INVAL;
	}

	amdsmi_bdf_t bdf = {0};
	bdf.bdf.domain_number = domain;
	bdf.bdf.bus_number = bus;
	bdf.bdf.device_number = device;
	bdf.bdf.function_number = function;
	amdsmi_processor_handle processor = NULL;
	amdsmi_status_t status = amdsmi_get_processor_handle_from_bdf(bdf, &processor);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	amdsmi_asic_info_t asic_info = {0};
	status = amdsmi_get_gpu_asic_info(processor, &asic_info);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	snprintf(market_name, AMDSMI_MAX_STRING_LENGTH, "%s", asic_info.market_name);
	return AMDSMI_STATUS_SUCCESS;
}

static amdsmi_status_t amdsmi_set_partition_for_bdf(const char *bdf_text,
                                                    unsigned int profile_index) {
	unsigned long long domain;
	unsigned int bus, device, function;
	char trailing;
	if (sscanf(bdf_text, "%llx:%x:%x.%x%c", &domain, &bus, &device, &function,
	           &trailing) != 4 || bus > 0xff || device > 0x1f || function > 7) {
		return AMDSMI_STATUS_INVAL;
	}

	amdsmi_bdf_t bdf = {0};
	bdf.bdf.domain_number = domain;
	bdf.bdf.bus_number = bus;
	bdf.bdf.device_number = device;
	bdf.bdf.function_number = function;
	amdsmi_processor_handle processor = NULL;
	amdsmi_status_t status = amdsmi_get_processor_handle_from_bdf(bdf, &processor);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	return amdsmi_set_gpu_accelerator_partition_profile(processor, profile_index);
}

static amdsmi_status_t amdsmi_partition_profiles_for_bdf(
		const char *bdf_text, amdsmi_accelerator_partition_profile_config_t *config) {
	unsigned long long domain;
	unsigned int bus, device, function;
	char trailing;
	if (sscanf(bdf_text, "%llx:%x:%x.%x%c", &domain, &bus, &device, &function,
	           &trailing) != 4 || bus > 0xff || device > 0x1f || function > 7) {
		return AMDSMI_STATUS_INVAL;
	}

	amdsmi_bdf_t bdf = {0};
	bdf.bdf.domain_number = domain;
	bdf.bdf.bus_number = bus;
	bdf.bdf.device_number = device;
	bdf.bdf.function_number = function;
	amdsmi_processor_handle processor = NULL;
	amdsmi_status_t status = amdsmi_get_processor_handle_from_bdf(bdf, &processor);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	return amdsmi_get_gpu_accelerator_partition_profile_config(processor, config);
}

static uint32_t amdsmi_nps_cap_mask(amdsmi_nps_caps_t caps) {
	return caps.nps_cap_mask;
}

static amdsmi_status_t amdsmi_memory_partition_for_bdf(const char *bdf_text,
                                                       char *memory_partition) {
	unsigned long long domain;
	unsigned int bus, device, function;
	char trailing;
	if (sscanf(bdf_text, "%llx:%x:%x.%x%c", &domain, &bus, &device, &function,
	           &trailing) != 4 || bus > 0xff || device > 0x1f || function > 7) {
		return AMDSMI_STATUS_INVAL;
	}

	amdsmi_bdf_t bdf = {0};
	bdf.bdf.domain_number = domain;
	bdf.bdf.bus_number = bus;
	bdf.bdf.device_number = device;
	bdf.bdf.function_number = function;
	amdsmi_processor_handle processor = NULL;
	amdsmi_status_t status = amdsmi_get_processor_handle_from_bdf(bdf, &processor);
	if (status != AMDSMI_STATUS_SUCCESS) {
		return status;
	}
	return amdsmi_get_gpu_memory_partition(processor, memory_partition,
	                                       AMDSMI_MAX_STRING_LENGTH);
}
*/
import "C"

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

var amdSMIMu sync.Mutex

// amdSMICache serves AMD SMI lookups that are static per card layout (device
// UUID, product name, partition profiles): the first call fetches and stores
// them, later calls serve the cache and only fetch BDFs never seen before
// (e.g. a GPU hotplugged after boot). The current memory partition is NOT
// cached: it changes with the partition configuration.
type amdSMICache[T any] struct {
	mu      sync.Mutex
	data    map[string]T
	fetcher func([]string) (map[string]T, error)
}

func newAMDSCache[T any](fetcher func([]string) (map[string]T, error)) *amdSMICache[T] {
	return &amdSMICache[T]{data: make(map[string]T), fetcher: fetcher}
}

func (c *amdSMICache[T]) Get(bdfs []string) (map[string]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	missing := make([]string, 0, len(bdfs))
	for _, bdf := range bdfs {
		if _, ok := c.data[bdf]; !ok {
			missing = append(missing, bdf)
		}
	}
	if len(missing) > 0 {
		fresh, err := c.fetcher(missing)
		for k, v := range fresh {
			c.data[k] = v
		}
		if err != nil {
			// Keep what succeeded; retry the failures on the next call.
			return c.data, err
		}
	}
	out := make(map[string]T, len(bdfs))
	for _, bdf := range bdfs {
		if v, ok := c.data[bdf]; ok {
			out[bdf] = v
		}
	}
	return out, nil
}

var (
	amdSMICUUIDs     = newAMDSCache(fetchAMDSUUIDs)
	amdSMIProduct    = newAMDSCache(fetchAMDSMIProductNames)
	amdSMIPartitions = newAMDSCache(fetchAMDGPUPartitionProfiles)
)

// GetAMDSMIUUIDs resolves AMD SMI UUIDs by PCI BDF, cached after the first
// call. AMD SMI initialization is process-global, so calls are serialized and
// always balanced with shut_down. An individual BDF failure does not discard
// UUIDs obtained for other devices.
func GetAMDSMIUUIDs(bdfs []string) (map[string]string, error) {
	return amdSMICUUIDs.Get(bdfs)
}

func fetchAMDSUUIDs(bdfs []string) (map[string]string, error) {
	amdSMIMu.Lock()
	defer amdSMIMu.Unlock()

	if status := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS); status != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_init: status %d", status)
	}
	defer C.amdsmi_shut_down()

	uuidByBDF := make(map[string]string, len(bdfs))
	var failures []string
	for _, rawBDF := range bdfs {
		rawBDF = strings.ToLower(strings.TrimSpace(rawBDF))
		bdf := normalizeBDF(rawBDF)
		if bdf == "" {
			continue
		}
		cBDF := C.CString(bdf)
		var uuid [C.AMDSMI_GPU_UUID_SIZE]C.char
		length := C.uint(C.AMDSMI_GPU_UUID_SIZE)
		status := C.amdsmi_uuid_for_bdf(cBDF, &uuid[0], &length)
		C.free(unsafe.Pointer(cBDF))
		if status != C.AMDSMI_STATUS_SUCCESS {
			failures = append(failures, fmt.Sprintf("%s (status %d)", bdf, status))
			continue
		}
		if value := strings.TrimSpace(C.GoString(&uuid[0])); value != "" {
			uuidByBDF[bdf] = value
			// Keep the source spelling too: KFD topology represents the
			// function as a fourth colon-separated component.
			uuidByBDF[rawBDF] = value
		}
	}
	if len(failures) > 0 {
		return uuidByBDF, fmt.Errorf("AMD SMI UUID lookup failed for %s", strings.Join(failures, ", "))
	}
	return uuidByBDF, nil
}

// GetAMDSMIProductNames resolves the AMD SMI ASIC market_name by PCI BDF,
// cached after the first call. This is the user-facing product name reported
// in DeviceInfo.Type.
func GetAMDSMIProductNames(bdfs []string) (map[string]string, error) {
	return amdSMIProduct.Get(bdfs)
}

func fetchAMDSMIProductNames(bdfs []string) (map[string]string, error) {
	amdSMIMu.Lock()
	defer amdSMIMu.Unlock()

	if status := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS); status != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_init: status %d", status)
	}
	defer C.amdsmi_shut_down()

	namesByBDF := make(map[string]string, len(bdfs))
	var failures []string
	for _, rawBDF := range bdfs {
		rawBDF = strings.ToLower(strings.TrimSpace(rawBDF))
		bdf := normalizeBDF(rawBDF)
		if bdf == "" {
			continue
		}
		cBDF := C.CString(bdf)
		var marketName [C.AMDSMI_MAX_STRING_LENGTH]C.char
		status := C.amdsmi_product_name_for_bdf(cBDF, &marketName[0])
		C.free(unsafe.Pointer(cBDF))
		if status != C.AMDSMI_STATUS_SUCCESS {
			failures = append(failures, fmt.Sprintf("%s (status %d)", bdf, status))
			continue
		}
		if value := strings.TrimSpace(C.GoString(&marketName[0])); value != "" {
			namesByBDF[bdf] = value
			namesByBDF[rawBDF] = value
		}
	}
	if len(failures) > 0 {
		return namesByBDF, fmt.Errorf("AMD SMI product-name lookup failed for %s", strings.Join(failures, ", "))
	}
	return namesByBDF, nil
}

// GetAMDSCurrentMemoryPartitions resolves the current memory partition (NPS1,
// NPS2, ...) by PCI BDF through the AMD SMI C API, lowercased for the
// sysfs spelling used in topology device data.
func GetAMDSCurrentMemoryPartitions(bdfs []string) (map[string]string, error) {
	amdSMIMu.Lock()
	defer amdSMIMu.Unlock()

	if status := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS); status != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_init: status %d", status)
	}
	defer C.amdsmi_shut_down()

	partitionByBDF := make(map[string]string, len(bdfs))
	var failures []string
	for _, rawBDF := range bdfs {
		rawBDF = strings.ToLower(strings.TrimSpace(rawBDF))
		bdf := normalizeBDF(rawBDF)
		if bdf == "" {
			continue
		}
		cBDF := C.CString(bdf)
		var partition [C.AMDSMI_MAX_STRING_LENGTH]C.char
		status := C.amdsmi_memory_partition_for_bdf(cBDF, &partition[0])
		C.free(unsafe.Pointer(cBDF))
		if status != C.AMDSMI_STATUS_SUCCESS {
			failures = append(failures, fmt.Sprintf("%s (status %d)", bdf, status))
			continue
		}
		if value := strings.ToLower(strings.TrimSpace(C.GoString(&partition[0]))); value != "" {
			partitionByBDF[bdf] = value
			partitionByBDF[rawBDF] = value
		}
	}
	if len(failures) > 0 {
		return partitionByBDF, fmt.Errorf("AMD SMI memory-partition lookup failed for %s", strings.Join(failures, ", "))
	}
	return partitionByBDF, nil
}

// PartitionProfile describes one accelerator partition profile a GPU can be
// set to: the profile_index passed to amdsmi_set_gpu_accelerator_partition_profile,
// the partition type, the memory partition modes it supports and the resulting
// partition geometry.
type PartitionProfile struct {
	ProfileIndex    int
	Type            string
	MemoryCaps      string
	NumPartitions   int
	XCCPerPartition int
}

// GetAMDGPUPartitionProfiles resolves the accelerator partition profiles each
// GPU supports (SPX/DPX/QPX/CPX with partition counts), keyed by BDF. The
// profile list is static per card, so it is cached after the first call.
func GetAMDGPUPartitionProfiles(bdfs []string) (map[string][]PartitionProfile, error) {
	return amdSMIPartitions.Get(bdfs)
}

func fetchAMDGPUPartitionProfiles(bdfs []string) (map[string][]PartitionProfile, error) {
	amdSMIMu.Lock()
	defer amdSMIMu.Unlock()

	if status := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS); status != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("amdsmi_init: status %d", status)
	}
	defer C.amdsmi_shut_down()

	profilesByBDF := make(map[string][]PartitionProfile, len(bdfs))
	var failures []string
	for _, rawBDF := range bdfs {
		rawBDF = strings.ToLower(strings.TrimSpace(rawBDF))
		bdf := normalizeBDF(rawBDF)
		if bdf == "" {
			continue
		}
		profiles, err := partitionProfilesForBDF(bdf)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", bdf, err))
			continue
		}
		profilesByBDF[bdf] = profiles
		profilesByBDF[rawBDF] = profiles
	}
	if len(failures) > 0 {
		return profilesByBDF, fmt.Errorf("AMD SMI partition-profile lookup failed for %s", strings.Join(failures, ", "))
	}
	return profilesByBDF, nil
}

// partitionProfilesForBDF resolves the accelerator partition profiles for a
// single BDF. The caller holds amdSMIMu.
func partitionProfilesForBDF(bdf string) ([]PartitionProfile, error) {
	cBDF := C.CString(bdf)
	var config C.amdsmi_accelerator_partition_profile_config_t
	status := C.amdsmi_partition_profiles_for_bdf(cBDF, &config)
	C.free(unsafe.Pointer(cBDF))
	if status != C.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("partition-profile lookup failed (status %d)", status)
	}
	profiles := make([]PartitionProfile, 0, int(config.num_profiles))
	for i := 0; i < int(config.num_profiles); i++ {
		profile := config.profiles[i]
		profiles = append(profiles, PartitionProfile{
			ProfileIndex:    int(profile.profile_index),
			Type:            acceleratorPartitionTypeString(profile.profile_type),
			MemoryCaps:      npsCapsString(C.amdsmi_nps_cap_mask(profile.memory_caps)),
			NumPartitions:   int(profile.num_partitions),
			XCCPerPartition: xccPerPartition(config, profile.profile_index),
		})
	}
	return profiles, nil
}

// SetGPUComputePartitions flips every GPU to the given compute partition
// type (spx/dpx/qpx/cpx) through the AMD SMI C API. The profile index comes
// from the accelerator partition profiles each GPU supports. A GPU with
// running processes fails (busy) and is reported per BDF; the rest still
// flip. GPUs already in the target mode are left untouched.
func SetGPUComputePartitions(bdfs []string, computeType string) error {
	if len(bdfs) == 0 {
		return nil
	}
	amdSMIMu.Lock()
	defer amdSMIMu.Unlock()

	if status := C.amdsmi_init(C.AMDSMI_INIT_AMD_GPUS); status != C.AMDSMI_STATUS_SUCCESS {
		return fmt.Errorf("amdsmi_init: status %d", status)
	}
	defer C.amdsmi_shut_down()

	var failures []string
	seen := make(map[string]bool)
	for _, rawBDF := range bdfs {
		rawBDF = strings.ToLower(strings.TrimSpace(rawBDF))
		bdf := normalizeBDF(rawBDF)
		if bdf == "" || seen[bdf] {
			continue
		}
		seen[bdf] = true
		profiles, err := partitionProfilesForBDF(bdf)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (profiles: %v)", bdf, err))
			continue
		}
		profileIndex := -1
		for _, profile := range profiles {
			if strings.EqualFold(profile.Type, computeType) {
				profileIndex = profile.ProfileIndex
				break
			}
		}
		if profileIndex < 0 {
			failures = append(failures, fmt.Sprintf("%s (no %s profile)", bdf, computeType))
			continue
		}
		cBDF := C.CString(bdf)
		status := C.amdsmi_set_partition_for_bdf(cBDF, C.uint(profileIndex))
		C.free(unsafe.Pointer(cBDF))
		if status != C.AMDSMI_STATUS_SUCCESS {
			failures = append(failures, fmt.Sprintf("%s (status %d)", bdf, status))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("compute-partition set failed for %s", strings.Join(failures, ", "))
	}
	return nil
}

func xccPerPartition(config C.amdsmi_accelerator_partition_profile_config_t, profileIndex C.uint) int {
	for j := 0; j < int(config.num_resource_profiles); j++ {
		res := config.resource_profiles[j]
		if res.profile_index == profileIndex && res.resource_type == C.AMDSMI_ACCELERATOR_XCC {
			return int(res.partition_resource)
		}
	}
	return 0
}

func acceleratorPartitionTypeString(t C.amdsmi_accelerator_partition_type_t) string {
	switch t {
	case C.AMDSMI_ACCELERATOR_PARTITION_SPX:
		return "SPX"
	case C.AMDSMI_ACCELERATOR_PARTITION_DPX:
		return "DPX"
	case C.AMDSMI_ACCELERATOR_PARTITION_TPX:
		return "TPX"
	case C.AMDSMI_ACCELERATOR_PARTITION_QPX:
		return "QPX"
	case C.AMDSMI_ACCELERATOR_PARTITION_CPX:
		return "CPX"
	}
	return ""
}

func npsCapsString(mask C.uint) string {
	caps := make([]string, 0, 4)
	if mask&1 != 0 {
		caps = append(caps, "NPS1")
	}
	if mask&2 != 0 {
		caps = append(caps, "NPS2")
	}
	if mask&4 != 0 {
		caps = append(caps, "NPS4")
	}
	if mask&8 != 0 {
		caps = append(caps, "NPS8")
	}
	return strings.Join(caps, ",")
}

func normalizeBDF(bdf string) string {
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	// KFD topology uses domain:bus:device:function, while AMD SMI expects
	// the conventional PCI domain:bus:device.function form.
	parts := strings.Split(bdf, ":")
	if len(parts) == 4 && !strings.Contains(parts[3], ".") {
		return strings.Join(parts[:3], ":") + "." + parts[3]
	}
	return bdf
}
