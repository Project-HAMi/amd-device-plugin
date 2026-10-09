/*
Copyright 2024 The HAMi Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/golang/glog"
)

const (
	AssignedNodeAnnotations = "hami.io/vgpu-node"
	BindTimeAnnotations     = "hami.io/bind-time"
	DeviceBindPhase         = "hami.io/bind-phase"
	DeviceAllocation        = "hami.io/amd-devices-allocated"
	DeviceToAllocate        = "hami.io/amd-devices-to-allocate"
	// CuAllocation is JSON: { "<device-uuid>": "<ID_List>", ... } with device-uuid matching DeviceInfo.ID (e.g. node~card0).
	// ID_List format examples: "0-90", "0-3,8,10-12".
	CuAllocation = "hami.io/amd-cu-allocated"

	DeviceBindAllocating = "allocating"
	DeviceBindFailed     = "failed"
	DeviceBindSuccess    = "success"

	// NodeNameEnvName define env var name for use get node name.
	NodeNameEnvName = "NODE_NAME"
)

type DeviceInfo struct {
	ID           string         `json:"id,omitempty"`
	Index        uint           `json:"index,omitempty"`
	Count        int32          `json:"count,omitempty"`
	Devmem       int32          `json:"devmem,omitempty"`
	Devcore      int32          `json:"devcore,omitempty"`
	Type         string         `json:"type,omitempty"`
	Numa         int            `json:"numa,omitempty"`
	Mode         string         `json:"mode,omitempty"`
	Health       bool           `json:"health,omitempty"`
	DeviceVendor string         `json:"devicevendor,omitempty"`
	CustomInfo   map[string]any `json:"custominfo,omitempty"`
}

type ContainerDevice struct {
	UUID       string
	Type       string
	Usedmem    int32
	Usedcores  int32
	CustomInfo map[string]any
}

type ContainerDevices []ContainerDevice

type PodSingleDevice []ContainerDevices
type PodDevices map[string]PodSingleDevice

const (
	// OneContainerMultiDeviceSplitSymbol this is when one container use multi device, use : symbol to join device info.
	OneContainerMultiDeviceSplitSymbol = ":"

	// OnePodMultiContainerSplitSymbol this is when one pod having multi container and more than one container use device, use ; symbol to join device info.
	OnePodMultiContainerSplitSymbol = ";"
)

// InRequestDevices maps a device type to its to-allocate annotation.
var InRequestDevices = map[string]string{"amd": DeviceToAllocate}

func DecodeContainerDevices(str string) (ContainerDevices, error) {
	if len(str) == 0 {
		return ContainerDevices{}, nil
	}
	cd := strings.Split(str, OneContainerMultiDeviceSplitSymbol)
	contdev := ContainerDevices{}
	tmpdev := ContainerDevice{}
	glog.V(5).Infof("Start to decode container device %s", str)
	for _, val := range cd {
		if strings.Contains(val, ",") {
			tmpstr := strings.Split(val, ",")
			if len(tmpstr) < 4 {
				return ContainerDevices{}, fmt.Errorf("pod annotation format error; information missing, please do not use nodeName field in task")
			}
			tmpdev.UUID = tmpstr[0]
			tmpdev.Type = tmpstr[1]
			devmem, err := strconv.ParseInt(tmpstr[2], 10, 32)
			if err != nil {
				return ContainerDevices{}, fmt.Errorf("parse device memory %q: %w", tmpstr[2], err)
			}
			tmpdev.Usedmem = int32(devmem)
			devcores, err := strconv.ParseInt(tmpstr[3], 10, 32)
			if err != nil {
				return ContainerDevices{}, fmt.Errorf("parse device cores %q: %w", tmpstr[3], err)
			}
			tmpdev.Usedcores = int32(devcores)
			contdev = append(contdev, tmpdev)
		}
	}
	glog.V(5).Infof("Finished decoding container devices. Total devices: %d", len(contdev))
	return contdev, nil
}

func DecodePodDevices(checklist map[string]string, annos map[string]string) (PodDevices, error) {
	glog.V(5).Infof("checklist is [%+v], annos is [%+v]", checklist, annos)
	if len(annos) == 0 {
		return PodDevices{}, nil
	}
	pd := make(PodDevices)
	for devID, devs := range checklist {
		str, ok := annos[devs]
		if !ok {
			continue
		}
		pd[devID] = make(PodSingleDevice, 0)
		// Entries stay aligned with Spec.Containers, so empty ones are kept;
		// only the terminator written by EncodePodSingleDevice is dropped.
		for s := range strings.SplitSeq(strings.TrimSuffix(str, OnePodMultiContainerSplitSymbol), OnePodMultiContainerSplitSymbol) {
			cd, err := DecodeContainerDevices(s)
			if err != nil {
				return PodDevices{}, err
			}
			pd[devID] = append(pd[devID], cd)
		}
	}
	glog.V(5).Infof("Decoded pod annos, poddevices: %+v", pd)
	return pd, nil
}

func EncodeContainerDevices(cd ContainerDevices) string {
	tmp := ""
	for _, val := range cd {
		tmp += val.UUID + "," + val.Type + "," + strconv.Itoa(int(val.Usedmem)) + "," + strconv.Itoa(int(val.Usedcores)) + OneContainerMultiDeviceSplitSymbol
	}
	glog.Infof("Encoded container Devices: %s", tmp)
	return tmp
}

func EncodePodSingleDevice(pd PodSingleDevice) string {
	res := ""
	for _, ctrdevs := range pd {
		res += EncodeContainerDevices(ctrdevs) + OnePodMultiContainerSplitSymbol
	}
	glog.Infof("Encoded pod single devices %s", res)
	return res
}
