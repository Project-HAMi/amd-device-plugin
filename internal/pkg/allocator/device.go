/**
# Copyright 2025 Advanced Micro Devices, Inc. All rights reserved.
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

package allocator

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/glog"
)

const (
	topoRootPath = "/sys/class/kfd/kfd/topology/nodes"
)

// below scores/weights are used to determine the closeness/efficiency of communication between GPU pairs
const (
	// weight if GPUs/partitions belong to same GPU
	sameDevIdWeight = 10
	// weight if a pair is connected via XGMI link
	xgmiLinkWeight = 10
	// weight if GPU pair belongs to same numa node
	sameNumaNodeWeight = 10
	// weight if GPUs/partitions belong to different GPU
	differentDevIdWeight = 20
	// weight if GPU pair belongs to different numa node
	differentNumaNodeWeight = 20
	// weight if a pair is connected via PCIE link
	pcieLinkWeight = 40
	// weight if a pair is connected via any other link apart from XGMI or PCIE
	otherLinkWeight = 50
)

type Device struct {
	Id                   string
	NodeId               int
	NumaNode             int
	DevId                string
	Card                 int
	RenderD              int
	ComputePartitionType string
	MemoryPartitionType  string
}

type DeviceSet struct {
	Ids         []int
	TotalWeight int
	LastIdx     int
	Size        int
	ParentIds   []int
}

type DevicePartitions struct {
	ParentId string
	DevId    string
	Ids      []int
	Devs     []string
}

func setContainsAll[K int | string](set, subset []K) bool {
	if len(subset) > len(set) {
		return false
	}
	for _, dev := range subset {
		devFound := false
		for i := range set {
			if set[i] == dev {
				devFound = true
				break
			}
		}
		if !devFound {
			return false
		}
	}
	return true
}

func fetchTopoProperties(path string, re []*regexp.Regexp) ([]int, error) {
	f, e := os.Open(path)
	if e != nil {
		glog.Errorf("Unable to open properties file. Error:%v", e)
		return nil, e
	}
	defer func() { _ = f.Close() }()

	res := make([]int, len(re))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		for idx := range re {
			m := re[idx].FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			v, err := strconv.ParseInt(m[1], 0, 32)
			if err != nil {
				glog.Errorf("Unable to parse properties file. Error:%v", err)
				return nil, err
			}
			res[idx] = int(v)
		}
	}

	return res, nil
}

func calculatePairWeight(from, to *Device, linkType int) int {
	weight := differentDevIdWeight
	if from.DevId == to.DevId {
		weight = sameDevIdWeight
	}

	switch linkType {
	case 11: // xgmi
		weight += xgmiLinkWeight
	case 2: // PCIE
		weight += pcieLinkWeight
	default: // other link types are given higher weight
		weight += otherLinkWeight
	}

	if from.NumaNode == to.NumaNode {
		weight += sameNumaNodeWeight
	} else {
		weight += differentNumaNodeWeight
	}
	return weight
}

func scanAndPopulatePeerWeights(fromPath string, devices []*Device, lookupNodes map[int]struct{}, p2pWeights map[int]map[int]int) {
	// Glob only fails on a malformed pattern, and these are constant
	paths, _ := filepath.Glob(filepath.Join(fromPath, "io_links", "[0-9]*"))
	p2pPaths, _ := filepath.Glob(filepath.Join(fromPath, "p2p_links", "[0-9]*"))
	paths = append(paths, p2pPaths...)
	re := []*regexp.Regexp{
		regexp.MustCompile(`node_from\s(\d+)`),
		regexp.MustCompile(`node_to\s(\d+)`),
		regexp.MustCompile(`type\s(\d+)`),
	}
	for _, topath := range paths {
		propFile := filepath.Join(topath, "properties")
		vals, err := fetchTopoProperties(propFile, re)
		if err != nil {
			continue
		}
		// to avoid duplicates in the map we make sure from < to
		var from, to int
		if vals[0] < vals[1] {
			from = vals[0]
			to = vals[1]
		} else {
			from = vals[1]
			to = vals[0]
		}
		if _, ok := lookupNodes[from]; !ok {
			continue
		}
		if _, ok := lookupNodes[to]; !ok {
			continue
		}

		var fromDev, toDev *Device
		devsFound := false
		for idx := range devices {
			if devices[idx].NodeId == from {
				fromDev = devices[idx]
			}
			if devices[idx].NodeId == to {
				toDev = devices[idx]
			}
			if fromDev != nil && toDev != nil {
				devsFound = true
				break
			}
		}
		if devsFound {
			if _, ok := p2pWeights[from]; !ok {
				p2pWeights[from] = make(map[int]int)
			}
			p2pWeights[from][to] = calculatePairWeight(fromDev, toDev, vals[2])
		}
	}
}

func fetchAllPairWeights(devices []*Device, p2pWeights map[int]map[int]int, folderPath string) error {
	if len(devices) == 0 {
		errMsg := "devices list is empty, unable to calculate pair wise weights"
		glog.Info(errMsg)
		return errors.New(errMsg)
	}
	if folderPath == "" {
		folderPath = topoRootPath
	}
	paths, _ := filepath.Glob(filepath.Join(folderPath, "[0-9]*"))
	nodeIds := make(map[int]struct{})
	for idx := range devices {
		nodeIds[devices[idx].NodeId] = struct{}{}
	}
	drmRenderMinor := []*regexp.Regexp{regexp.MustCompile(`drm_render_minor\s(\d+)`)}
	for _, path := range paths {
		propFilePath := filepath.Join(path, "properties")
		vals, err := fetchTopoProperties(propFilePath, drmRenderMinor)
		// if drm_render_minor value is <= 0, then it's not a valid GPU/partition
		if err != nil || vals[0] <= 0 {
			continue
		}
		scanAndPopulatePeerWeights(path, devices, nodeIds, p2pWeights)
	}
	// GPUs with no direct io/p2p link (consumer cards, passthrough VMs) only
	// reach each other through the host, the costliest path.
	for i, a := range devices {
		for _, b := range devices[i+1:] {
			from, to := a, b
			if from.NodeId > to.NodeId {
				from, to = to, from
			}
			if from.NodeId == to.NodeId {
				continue
			}
			if _, ok := p2pWeights[from.NodeId][to.NodeId]; ok {
				continue
			}
			if p2pWeights[from.NodeId] == nil {
				p2pWeights[from.NodeId] = make(map[int]int)
			}
			p2pWeights[from.NodeId][to.NodeId] = calculatePairWeight(from, to, -1)
		}
	}
	return nil
}

func addDeviceToSubsetAndUpdateWeight(subset *DeviceSet, devId, devIdx int, p2pWeights map[int]map[int]int) *DeviceSet {
	currentWeight := subset.TotalWeight
	var from, to int
	ids := make([]int, 0, len(subset.Ids)+1)
	for _, d := range subset.Ids {
		if d < devId {
			from = d
			to = devId
		} else {
			from = devId
			to = d
		}
		currentWeight += p2pWeights[from][to]
	}
	ids = append(ids, subset.Ids...)
	ids = append(ids, devId)

	newSubset := NewDeviceSet(ids, subset.ParentIds, currentWeight, devIdx)
	return newSubset
}

func NewDeviceSet(nodeIds, parentIds []int, weight, lastIdx int) *DeviceSet {
	return &DeviceSet{
		Ids:         nodeIds,
		TotalWeight: weight,
		LastIdx:     lastIdx,
		Size:        len(nodeIds),
		ParentIds:   parentIds,
	}
}

// in case gpu is partitioned, we group partitions belonging to same gpu/device
// preference is to allocate maximum partitions from same gpu
func groupPartitionsByDevId(devs []*Device) map[string]*DevicePartitions {
	partitions := make(map[string]*DevicePartitions)
	for _, dev := range devs {
		if _, ok := partitions[dev.DevId]; !ok {
			partitions[dev.DevId] = &DevicePartitions{
				DevId: dev.DevId,
				Ids:   make([]int, 0),
				Devs:  make([]string, 0),
			}
		}
		if !strings.Contains(dev.Id, "amdgpu_xcp") {
			partitions[dev.DevId].ParentId = dev.Id
		}
		partitions[dev.DevId].Ids = append(partitions[dev.DevId].Ids, dev.NodeId)
		partitions[dev.DevId].Devs = append(partitions[dev.DevId].Devs, dev.Id)
	}
	return partitions
}

// from all the available partitions, we pick only required ones
// available represents the available/unallocated devices when the allocate request is called
// required represents the devices that are required to be allocated
// we filter out required ones as they are included in output set by default. removing them saves us computation time
// split devices of one GPU share a NodeId, so ids are counted rather than deduplicated
func filterPartitions(partitions map[string]*DevicePartitions, available, required []*Device) []*DevicePartitions {
	remaining := make(map[int]int)
	outset := make([]*DevicePartitions, 0)
	for _, av := range available {
		remaining[av.NodeId]++
	}
	for _, req := range required {
		remaining[req.NodeId]--
	}
	for _, partitionSet := range partitions {
		filteredIds := make([]int, 0)
		for _, id := range partitionSet.Ids {
			if remaining[id] > 0 {
				remaining[id]--
				filteredIds = append(filteredIds, id)
			}
		}
		if len(filteredIds) > 0 {
			sort.Slice(filteredIds, func(i, j int) bool {
				return filteredIds[i] < filteredIds[j]
			})
			filteredPartition := &DevicePartitions{
				DevId:    partitionSet.DevId,
				Ids:      filteredIds,
				ParentId: partitionSet.ParentId,
			}
			outset = append(outset, filteredPartition)
		}
	}
	sort.Slice(outset, func(i, j int) bool {
		len1 := len(outset[i].Ids)
		len2 := len(outset[j].Ids)
		if len1 == len2 {
			return outset[i].ParentId < outset[j].ParentId
		}
		return len1 < len2
	})
	return outset
}

func getCandidateDeviceSubsets(allDevPartitions map[string]*DevicePartitions, available, required []*Device, size int, p2pWeights map[int]map[int]int) ([]*DeviceSet, error) {
	if size <= 0 {
		return []*DeviceSet{}, fmt.Errorf("subset size should be positive integer")
	}

	if len(available) < size {
		return []*DeviceSet{}, fmt.Errorf("subset size is more than available devices")
	}

	sort.Slice(available, func(i, j int) bool {
		return available[i].NodeId < available[j].NodeId
	})

	// filterPartitions - partitions from same gpu are grouped into one set.
	// the sets are sorted in ascending order of partitions available for allocation.
	devPartitions := filterPartitions(allDevPartitions, available, required)
	newSize := size - len(required)
	subsetsTemp := make([]*DeviceSet, 0)
	subsetsFinal := make([]*DeviceSet, 0)
	// if the requested size is less than available partitions of a single gpu, try to allocate all from same gpu
	// subsetsFinal - contains candidate set that has requested number of gpus/partitions
	// subsetsTemp - if one gpu can not suffice requested number of partitions, we store in subsetsTemp
	for idx, partition := range devPartitions {
		ids := []int{partition.Ids[0]}
		parentIds := []int{idx}
		devset := NewDeviceSet(ids, parentIds, 0, idx)
		if newSize == 1 {
			for _, req := range required {
				devset = addDeviceToSubsetAndUpdateWeight(devset, req.NodeId, idx, p2pWeights)
			}
			subsetsFinal = append(subsetsFinal, devset)
			continue
		}
		sizeFulfilled := false
		for i := 1; i < len(partition.Ids); i++ {
			devset = addDeviceToSubsetAndUpdateWeight(devset, partition.Ids[i], idx, p2pWeights)
			if i == newSize-1 {
				sizeFulfilled = true
				break
			}
		}
		if sizeFulfilled {
			for _, req := range required {
				devset = addDeviceToSubsetAndUpdateWeight(devset, req.NodeId, idx, p2pWeights)
			}
			subsetsFinal = append(subsetsFinal, devset)
		} else {
			subsetsTemp = append(subsetsTemp, devset)
		}
	}
	// for each subsetsTemp, we loop over all the devPartitions
	// pick partitions from other gpu until the subsetsTemp has requested number of gpus/partitions
	for len(subsetsTemp) > 0 {
		currentSubset := subsetsTemp[0]
		subsetsTemp = subsetsTemp[1:]
		if len(currentSubset.ParentIds) == len(devPartitions) {
			continue
		}
		// devPartitions is sorted in ascending order of available partitions.
		// when we loop over to pick a candidate set, preference is given to gpus with lesser partitions available.
		// this way we can avoid fragmentation of gpus
		for idx := 0; idx < len(devPartitions); idx++ {
			// if current subset already has partitions from the current gpu, skip adding them again
			if slices.Contains(currentSubset.ParentIds, idx) {
				continue
			}
			// gpus taken whole are added in increasing index order so every set is built once;
			// only a gpu that is taken partially to finish the set may come from a lower index
			if idx < currentSubset.LastIdx && len(devPartitions[idx].Ids) <= newSize-currentSubset.Size {
				continue
			}
			parentIds := make([]int, 0, len(currentSubset.ParentIds)+1)
			parentIds = append(parentIds, currentSubset.ParentIds...)
			parentIds = append(parentIds, idx)
			devset := NewDeviceSet(currentSubset.Ids, parentIds, currentSubset.TotalWeight, currentSubset.LastIdx)
			for _, id := range devPartitions[idx].Ids {
				devset = addDeviceToSubsetAndUpdateWeight(devset, id, idx, p2pWeights)
				if devset.Size == newSize {
					for _, req := range required {
						devset = addDeviceToSubsetAndUpdateWeight(devset, req.NodeId, idx, p2pWeights)
					}
					subsetsFinal = append(subsetsFinal, devset)
					break
				}
			}
			if devset.Size < newSize {
				subsetsTemp = append(subsetsTemp, devset)
			}
		}
	}
	return subsetsFinal, nil
}
