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
	AssignedTimeAnnotations = "hami.io/vgpu-time"
	AssignedNodeAnnotations = "hami.io/vgpu-node"
	BindTimeAnnotations     = "hami.io/bind-time"
	DeviceBindPhase         = "hami.io/bind-phase"
	DeviceAllocation        = "hami.io/amd-devices-allocated"
	DeviceToAllocate        = "hami.io/amd-devices-to-allocate"
	// CuAllocation is JSON: { "<device-uuid>": "<hex-bitmap>", ... } with device-uuid matching DeviceInfo.ID (e.g. node~PCI-BDF).
	CuAllocation = "hami.io/amd-cu-allocated"

	DeviceBindAllocating = "allocating"
	DeviceBindFailed     = "failed"
	DeviceBindSuccess    = "success"

	DeviceLimit = 100
	//TimeLayout = "ANSIC"
	//DefaultTimeout = time.Second * 60.

	BestEffort string = "best-effort"
	Restricted string = "restricted"
	Guaranteed string = "guaranteed"

	// NodeNameEnvName define env var name for use get node name.
	NodeNameEnvName = "NODE_NAME"
	TaskPriority    = "CUDA_TASK_PRIORITY"
	CoreLimitSwitch = "GPU_CORE_UTILIZATION_POLICY"

	// LeaderRoleLabel is the label key used to identify the leader pod.
	HAMiRoleLabel = "hami.io/scheduler-role"
	// LeaderRoleLabelValueLeader is the label value used to identify the leader pod.
	HAMiRoleLabelValueLeader = "leader"
	// LeaderRoleLabelValueFollower is the label value used to identify the follower pod.
	HAMiRoleLabelValueFollower = "follower"

	// HAMiSchedulerLabelValue is the label key to identify the component of HAMi in Kubernetes.
	HAMiComponentLabel = "app.kubernetes.io/component"
	// HAMiComponentScheduler the label value for hami-scheduler.
	HAMiComponentScheduler = "hami-scheduler"
)

var (
	DebugMode         bool
	NodeName          string
	RuntimeSocketFlag string
)

type SchedulerPolicyName string

const (
	// NodeSchedulerPolicyBinpack is node use binpack scheduler policy.
	NodeSchedulerPolicyBinpack SchedulerPolicyName = "binpack"
	// NodeSchedulerPolicySpread is node use spread scheduler policy.
	NodeSchedulerPolicySpread SchedulerPolicyName = "spread"
	// GPUSchedulerPolicyBinpack is GPU use binpack scheduler.
	GPUSchedulerPolicyBinpack SchedulerPolicyName = "binpack"
	// GPUSchedulerPolicySpread is GPU use spread scheduler.
	GPUSchedulerPolicySpread SchedulerPolicyName = "spread"
	// GPUSchedulerPolicyTopology is GPU use topology scheduler.
	GPUSchedulerPolicyTopology SchedulerPolicyName = "topology-aware"
)

const (
	// NodeSchedulerPolicyAnnotationKey is user set Pod annotation to change this default node policy.
	NodeSchedulerPolicyAnnotationKey = "hami.io/node-scheduler-policy"
	// GPUSchedulerPolicyAnnotationKey is user set Pod annotation to change this default GPU policy.
	GPUSchedulerPolicyAnnotationKey = "hami.io/gpu-scheduler-policy"
)

func (s SchedulerPolicyName) String() string {
	return string(s)
}

const (
	Weight int = 10
)

type DeviceInfo struct {
	ID              string          `json:"id,omitempty"`
	Index           uint            `json:"index,omitempty"`
	Count           int32           `json:"count,omitempty"`
	Devmem          int32           `json:"devmem,omitempty"`
	Devcore         int32           `json:"devcore,omitempty"`
	Type            string          `json:"type,omitempty"`
	Numa            int             `json:"numa,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	Health          bool            `json:"health,omitempty"`
	DeviceVendor    string          `json:"devicevendor,omitempty"`
	CustomInfo      map[string]any  `json:"custominfo,omitempty"`
}

type ContainerDevice struct {
	// TODO current Idx cannot use, because EncodeContainerDevices method not encode this filed.
	Idx        int
	UUID       string
	Type       string
	Usedmem    int32
	Usedcores  int32
	CustomInfo map[string]any
}

type ContainerDeviceRequest struct {
	Nums             int32
	Type             string
	Memreq           int32
	MemPercentagereq int32
	Coresreq         int32
}

type ContainerDevices []ContainerDevice
type ContainerDeviceRequests map[string]ContainerDeviceRequest

// type ContainerAllDevices map[string]ContainerDevices.
type PodSingleDevice []ContainerDevices
type PodDeviceRequests []ContainerDeviceRequests
type PodDevices map[string]PodSingleDevice

const (
	// OneContainerMultiDeviceSplitSymbol this is when one container use multi device, use : symbol to join device info.
	OneContainerMultiDeviceSplitSymbol = ":"

	// OnePodMultiContainerSplitSymbol this is when one pod having multi container and more than one container use device, use ; symbol to join device info.
	OnePodMultiContainerSplitSymbol = ";"
)

var (
	GPUSchedulerPolicy string
	InRequestDevices   map[string]string
	SupportDevices     map[string]string
)

func init() {
	InRequestDevices = make(map[string]string)
	InRequestDevices["amd"] = DeviceToAllocate
	SupportDevices = make(map[string]string)
	SupportDevices["amd"] = DeviceAllocation
}

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
			//fmt.Println("cd is ", val)
			tmpstr := strings.Split(val, ",")
			if len(tmpstr) < 4 {
				return ContainerDevices{}, fmt.Errorf("pod annotation format error; information missing, please do not use nodeName field in task")
			}
			tmpdev.UUID = tmpstr[0]
			tmpdev.Type = tmpstr[1]
			devmem, _ := strconv.ParseInt(tmpstr[2], 10, 32)
			tmpdev.Usedmem = int32(devmem)
			devcores, _ := strconv.ParseInt(tmpstr[3], 10, 32)
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
		for s := range strings.SplitSeq(str, OnePodMultiContainerSplitSymbol) {
			cd, err := DecodeContainerDevices(s)
			if err != nil {
				return PodDevices{}, nil
			}
			if len(cd) == 0 {
				continue
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
	//return strings.Join(cd, ",")
}

func EncodeContainerDeviceType(cd ContainerDevices, t string) string {
	tmp := ""
	for _, val := range cd {
		if strings.Compare(val.Type, t) == 0 {
			tmp += val.UUID + "," + val.Type + "," + strconv.Itoa(int(val.Usedmem)) + "," + strconv.Itoa(int(val.Usedcores))
		}
		tmp += OneContainerMultiDeviceSplitSymbol
	}
	glog.Infof("Encoded container Certain Device type: %s->%s", t, tmp)
	return tmp
}

func EncodePodSingleDevice(pd PodSingleDevice) string {
	res := ""
	for _, ctrdevs := range pd {
		res = res + EncodeContainerDevices(ctrdevs)
		res = res + OnePodMultiContainerSplitSymbol
	}
	glog.Infof("Encoded pod single devices %s", res)
	return res
}

func EncodePodDevices(checklist map[string]string, pd PodDevices) map[string]string {
	res := map[string]string{}
	for devType, cd := range pd {
		glog.Infoln("devtype=", devType)
		res[checklist[devType]] = EncodePodSingleDevice(cd)
	}
	glog.Infof("Encoded pod Devices %s\n", res)
	return res
}