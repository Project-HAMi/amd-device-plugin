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
*/
import "C"

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

var amdSMIMu sync.Mutex

// GetAMDSMIUUIDs resolves AMD SMI UUIDs by PCI BDF. AMD SMI initialization is
// process-global, so calls are serialized and always balanced with shut_down.
// An individual BDF failure does not discard UUIDs obtained for other devices.
func GetAMDSMIUUIDs(bdfs []string) (map[string]string, error) {
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
