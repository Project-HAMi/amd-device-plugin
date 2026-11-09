/*
Copyright 2026 The HAMi Authors.

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

package vgpumonitor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testBDF  = "0000:06:00.0"
	testUUID = "33ff7590-0000-1000-8021-340866ba8c47"
	testUID  = "76ee6770-1f4c-436c-9d68-2c6d515ad8f7"
	region   = "drm/" + testBDF + "/vram"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// fixture lays out one amdgpu card, one iGPU the plugin does not register, and
// one container holding 1 GiB on the card.
func fixture(t *testing.T) *Collector {
	t.Helper()
	drm, cg := t.TempDir(), t.TempDir()
	card := filepath.Join(drm, "card1", "device")
	writeFile(t, filepath.Join(card, "uevent"), "DRIVER=amdgpu\nPCI_SLOT_NAME="+testBDF+"\n")
	writeFile(t, filepath.Join(card, "mem_info_vram_used"), "1251446784\n")
	writeFile(t, filepath.Join(card, "mem_info_vram_total"), "17095983104\n")
	writeFile(t, filepath.Join(card, "gpu_busy_percent"), "37\n")
	writeFile(t, filepath.Join(card, "hwmon", "hwmon3", "temp1_input"), "42000\n")
	writeFile(t, filepath.Join(card, "hwmon", "hwmon3", "power1_average"), "11000000\n")
	igpu := filepath.Join(drm, "card2", "device")
	writeFile(t, filepath.Join(igpu, "uevent"), "DRIVER=amdgpu\nPCI_SLOT_NAME=0000:0e:00.0\n")
	writeFile(t, filepath.Join(igpu, "mem_info_vram_used"), "1\n")
	writeFile(t, filepath.Join(igpu, "gpu_busy_percent"), "1\n")

	scope := filepath.Join(cg, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice", "cri-containerd-abc123.scope")
	writeFile(t, filepath.Join(scope, "dmem.current"), region+" 1073741824\n")
	writeFile(t, filepath.Join(scope, "dmem.max"), region+" max\n")

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Annotations: map[string]string{
		"hami.io/node-amd-register": `[{"id":"` + testUUID + `","index":1,"type":"AMD Radeon Graphics","custominfo":{"pciBDF":"` + testBDF + `"}}]`,
	}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "burn", Namespace: "amd-e2e", UID: testUID},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "burn", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"amd.com/gpu": resource.MustParse("1"), "amd.com/gpumem": resource.MustParse("4096")}}},
			{Name: "sidecar"},
		}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "burn", ContainerID: "containerd://abc123"},
			{Name: "sidecar", ContainerID: "containerd://def456"},
		}},
	}
	return &Collector{
		NodeName: "gpu-1", CgroupRoot: cg, DRMRoot: drm,
		Pods: func() ([]*corev1.Pod, error) { return []*corev1.Pod{pod}, nil },
		Node: func() (*corev1.Node, error) { return node, nil },
	}
}

func TestCollect(t *testing.T) {
	c := fixture(t)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	want := `
# HELP hami_host_gpu_memory_used_bytes GPU device memory usage in bytes
# TYPE hami_host_gpu_memory_used_bytes gauge
hami_host_gpu_memory_used_bytes{device_index="1",device_type="AMD Radeon Graphics",device_uuid="` + testUUID + `",node="gpu-1"} 1.251446784e+09
# HELP hami_host_gpu_memory_total_bytes GPU device memory capacity in bytes
# TYPE hami_host_gpu_memory_total_bytes gauge
hami_host_gpu_memory_total_bytes{device_index="1",device_type="AMD Radeon Graphics",device_uuid="` + testUUID + `",node="gpu-1"} 1.7095983104e+10
# HELP hami_host_gpu_power_usage_watts GPU power draw in watts
# TYPE hami_host_gpu_power_usage_watts gauge
hami_host_gpu_power_usage_watts{device_index="1",device_type="AMD Radeon Graphics",device_uuid="` + testUUID + `",node="gpu-1"} 11
# HELP hami_host_gpu_temperature_celsius GPU temperature in degrees Celsius
# TYPE hami_host_gpu_temperature_celsius gauge
hami_host_gpu_temperature_celsius{device_index="1",device_type="AMD Radeon Graphics",device_uuid="` + testUUID + `",node="gpu-1"} 42
# HELP hami_host_gpu_utilization_ratio GPU core utilization ratio (0-100)
# TYPE hami_host_gpu_utilization_ratio gauge
hami_host_gpu_utilization_ratio{device_index="1",device_type="AMD Radeon Graphics",device_uuid="` + testUUID + `",node="gpu-1"} 37
# HELP hami_vgpu_memory_limit_bytes vGPU device memory limit in bytes
# TYPE hami_vgpu_memory_limit_bytes gauge
hami_vgpu_memory_limit_bytes{container="burn",device_uuid="` + testUUID + `",namespace="amd-e2e",pod="burn",vdevice_index="0"} 4.294967296e+09
# HELP hami_vgpu_memory_used_bytes vGPU device memory usage in bytes
# TYPE hami_vgpu_memory_used_bytes gauge
hami_vgpu_memory_used_bytes{container="burn",device_uuid="` + testUUID + `",namespace="amd-e2e",pod="burn",vdevice_index="0"} 1.073741824e+09
# HELP hami_vgpumonitor_collect_success Whether the last metrics collection fully succeeded (1) or any part of it failed (0)
# TYPE hami_vgpumonitor_collect_success gauge
hami_vgpumonitor_collect_success{node="gpu-1"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want)))
}

func TestCollectAllocatedMemoryLimit(t *testing.T) {
	for _, tt := range []struct {
		name       string
		allocation string
		explicit   bool
		percentage bool
		dmemCap    string
		want       float64
	}{
		{name: "percentage", allocation: testUUID + ",AMD,4096,25:;", percentage: true, want: 4096 * mib},
		{name: "default memory", allocation: testUUID + ",AMD,8192,25:;", want: 8192 * mib},
		{name: "allocation precedes request", allocation: testUUID + ",AMD,2048,25:;", explicit: true, want: 2048 * mib},
		{name: "container cap precedes allocation", allocation: testUUID + ",AMD,4096,25:;", dmemCap: "1234", want: 1234},
		{name: "missing annotation", percentage: true},
		{name: "missing GPU", allocation: "another-gpu,AMD,8192,25:;", percentage: true},
		{name: "missing container slot", allocation: ";"},
		{name: "zero allocation", allocation: testUUID + ",AMD,0,25:;"},
		{name: "negative allocation", allocation: testUUID + ",AMD,-1,25:;"},
		{name: "malformed annotation", allocation: testUUID + ",AMD,invalid,25:;", explicit: true, want: 4096 * mib},
		{name: "truncated annotation", allocation: testUUID + ",AMD,4096:;", percentage: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := fixture(t)
			pods, err := c.Pods()
			require.NoError(t, err)
			pod := pods[0]
			if !tt.explicit {
				delete(pod.Spec.Containers[0].Resources.Limits, memoryResource)
			}
			if tt.percentage {
				pod.Spec.Containers[0].Resources.Limits["amd.com/gpumem-percentage"] = resource.MustParse("25")
			}
			pod.Annotations = map[string]string{
				"hami.io/amd-devices-allocated": tt.allocation,
				// Allocate consumes this annotation; it is not the persistent source.
				"hami.io/amd-devices-to-allocate": ";;",
			}
			if tt.allocation == "" {
				delete(pod.Annotations, "hami.io/amd-devices-allocated")
			}
			podCgroup := filepath.Join(c.CgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
				"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice")
			// A parent cap must not substitute for an absent container allocation.
			writeFile(t, filepath.Join(podCgroup, "dmem.max"), region+" 4294967296\n")
			if tt.dmemCap != "" {
				writeFile(t, filepath.Join(podCgroup, "cri-containerd-abc123.scope", "dmem.max"), region+" "+tt.dmemCap+"\n")
			}
			want := map[string]float64{}
			if tt.want > 0 {
				want["burn/"+testUUID] = tt.want
			}
			require.Equal(t, want, containerMemoryLimits(t, c))
		})
	}
}

func TestCollectAllocatedMemoryLimitsMatchContainerSlotsAndDeviceUUIDs(t *testing.T) {
	c := fixture(t)
	pods, err := c.Pods()
	require.NoError(t, err)
	pod := pods[0]
	request := corev1.ResourceRequirements{Limits: corev1.ResourceList{
		"amd.com/gpu": resource.MustParse("1"), "amd.com/gpumem-percentage": resource.MustParse("25"),
	}}
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup"}, {Name: "gpu-init", Resources: request}}
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{
		"amd.com/gpu": resource.MustParse("2"), "amd.com/gpumem-percentage": resource.MustParse("25"),
	}}
	pod.Spec.Containers[1].Resources = request
	pod.Spec.Containers = append([]corev1.Container{{Name: "plain"}}, pod.Spec.Containers...)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "gpu-init", ContainerID: "containerd://init123"}}
	// Empty slots belong to setup and plain. GPU order differs from PCI region order.
	pod.Annotations = map[string]string{"hami.io/amd-devices-allocated": ";" +
		testUUID + ",AMD,1024,25:;;" +
		"u-0e,AMD,8192,25:" + testUUID + ",AMD,4096,25:;" +
		testUUID + ",AMD,2048,25:;"}
	node, err := c.Node()
	require.NoError(t, err)
	node.Annotations["hami.io/node-amd-register"] = `[` +
		`{"id":"` + testUUID + `","index":1,"type":"AMD","custominfo":{"pciBDF":"` + testBDF + `"}},` +
		`{"id":"u-0e","index":2,"type":"AMD","custominfo":{"pciBDF":"0000:0e:00.0"}}]`
	writeFile(t, filepath.Join(c.DRMRoot, "card2", "device", "mem_info_vram_total"), "34359738368\n")
	podCgroup := filepath.Join(c.CgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice")
	writeFile(t, filepath.Join(podCgroup, "dmem.max"), region+" 7516192768\n")
	for id, current := range map[string]string{
		"abc123":  region + " 10\ndrm/0000:0e:00.0/vram 20\n",
		"def456":  region + " 30\n",
		"init123": region + " 40\n",
	} {
		writeFile(t, filepath.Join(podCgroup, "cri-containerd-"+id+".scope", "dmem.current"), current)
		writeFile(t, filepath.Join(podCgroup, "cri-containerd-"+id+".scope", "dmem.max"), region+" max\ndrm/0000:0e:00.0/vram max\n")
	}
	require.Equal(t, map[string]float64{
		"gpu-init/" + testUUID: 1024 * mib,
		"burn/" + testUUID:     4096 * mib,
		"burn/u-0e":            8192 * mib,
		"sidecar/" + testUUID:  2048 * mib,
	}, containerMemoryLimits(t, c))
}

func containerMemoryLimits(t *testing.T, c *Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	require.NoError(t, err)
	limits := map[string]float64{}
	for _, family := range families {
		switch family.GetName() {
		case "hami_vgpu_memory_limit_bytes":
			for _, metric := range family.GetMetric() {
				labels := map[string]string{}
				for _, label := range metric.GetLabel() {
					labels[label.GetName()] = label.GetValue()
				}
				limits[labels["container"]+"/"+labels["device_uuid"]] = metric.GetGauge().GetValue()
			}
		case "hami_vgpu_memory_used_bytes":
			require.NotEmpty(t, family.GetMetric())
		case "hami_vgpumonitor_collect_success":
			require.Equal(t, 1.0, family.GetMetric()[0].GetGauge().GetValue())
		}
	}
	return limits
}

func TestCollectWithoutSensorsStillSucceeds(t *testing.T) {
	c := fixture(t)
	require.NoError(t, os.RemoveAll(filepath.Join(c.DRMRoot, "card1", "device", "hwmon")))
	require.InDelta(t, 1, successOf(t, c), 0.001)
}

func TestCollectReportsFailures(t *testing.T) {
	t.Run("node without the registration annotation", func(t *testing.T) {
		c := fixture(t)
		c.Node = func() (*corev1.Node, error) { return &corev1.Node{}, nil }
		require.InDelta(t, 0, successOf(t, c), 0.001)
	})
	t.Run("node lookup fails", func(t *testing.T) {
		c := fixture(t)
		c.Node = func() (*corev1.Node, error) { return nil, errors.New("boom") }
		require.InDelta(t, 0, successOf(t, c), 0.001)
	})
	t.Run("pod lookup fails", func(t *testing.T) {
		c := fixture(t)
		c.Pods = func() ([]*corev1.Pod, error) { return nil, errors.New("boom") }
		require.InDelta(t, 0, successOf(t, c), 0.001)
	})
	t.Run("unreadable device memory", func(t *testing.T) {
		c := fixture(t)
		require.NoError(t, os.Remove(filepath.Join(c.DRMRoot, "card1", "device", "mem_info_vram_used")))
		require.InDelta(t, 0, successOf(t, c), 0.001)
	})
	t.Run("container on an unregistered device", func(t *testing.T) {
		c := fixture(t)
		scope := filepath.Join(c.CgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
			"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice", "cri-containerd-abc123.scope")
		writeFile(t, filepath.Join(scope, "dmem.current"), "drm/0000:0e:00.0/vram 5\n")
		require.InDelta(t, 0, successOf(t, c), 0.001)
	})
}

func TestContainerWithTwoCardsNumbersThemInOrderAndDmemCapWins(t *testing.T) {
	c := fixture(t)
	scope := filepath.Join(c.CgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice", "cri-containerd-abc123.scope")
	writeFile(t, filepath.Join(scope, "dmem.current"), "drm/0000:0e:00.0/vram 7\n"+region+" 9\n")
	writeFile(t, filepath.Join(scope, "dmem.max"), "drm/0000:0e:00.0/vram 100\n"+region+" 200\n")
	node, _ := c.Node()
	node.Annotations["hami.io/node-amd-register"] = `[` +
		`{"id":"u-06","index":1,"type":"AMD","custominfo":{"pciBDF":"` + testBDF + `"}},` +
		`{"id":"u-0e","index":0,"type":"AMD","custominfo":{"pciBDF":"0000:0e:00.0"}}]`

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "hami_vgpu_memory_limit_bytes" {
			continue
		}
		for _, m := range mf.GetMetric() {
			got[m.GetLabel()[4].GetValue()+"/"+m.GetLabel()[1].GetValue()] = m.GetGauge().GetValue()
		}
	}
	// Regions sort by PCI address, so 0000:06 is device 0 and 0000:0e is device 1,
	// and the dmem cap replaces the 4096 MiB the pod asked for.
	require.Equal(t, map[string]float64{"0/u-06": 200, "1/u-0e": 100}, got)
}

func successOf(t *testing.T, c *Collector) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "hami_vgpumonitor_collect_success" {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("collect_success not emitted")
	return 0
}

// A dmem region that is not "drm/<pci address>/<region>" must be skipped, not
// panic the scrape, and does not make the collection fail.
func TestCollectSkipsRegionsThatAreNotDRMCards(t *testing.T) {
	c := fixture(t)
	scope := filepath.Join(c.CgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod76ee6770_1f4c_436c_9d68_2c6d515ad8f7.slice", "cri-containerd-abc123.scope")
	writeFile(t, filepath.Join(scope, "dmem.current"), "noslash 5\ndrm/onlytwo 6\nother/0000:06:00.0/vram 7\n"+region+" 8\n")
	writeFile(t, filepath.Join(scope, "dmem.max"), region+" max\n")

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	used := 0
	for _, mf := range mfs {
		switch mf.GetName() {
		case "hami_vgpu_memory_used_bytes":
			used = len(mf.GetMetric())
			require.InDelta(t, 8, mf.GetMetric()[0].GetGauge().GetValue(), 0.001)
		case "hami_vgpumonitor_collect_success":
			require.InDelta(t, 1, mf.GetMetric()[0].GetGauge().GetValue(), 0.001)
		}
	}
	require.Equal(t, 1, used)
}
