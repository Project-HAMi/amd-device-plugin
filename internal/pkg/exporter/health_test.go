package exporter

import (
	"os"
	"path/filepath"
	"testing"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestApplyHealthMatchesSplitsByBDF(t *testing.T) {
	devs := []*pluginapi.Device{{ID: "0000:06:00.0#0"}, {ID: "0000:06:00.0#1"}, {ID: "0000:07:00.0#0"}}
	applyHealth(devs, map[string]string{"0000:06:00.0": pluginapi.Unhealthy}, pluginapi.Healthy, partitionBDF(t.TempDir(), t.TempDir()))
	want := []string{pluginapi.Unhealthy, pluginapi.Unhealthy, pluginapi.Healthy}
	for i, d := range devs {
		if d.Health != want[i] {
			t.Errorf("%s: health %s, want %s", d.ID, d.Health, want[i])
		}
	}
}

func TestApplyHealthMapsPartitionToParentGPU(t *testing.T) {
	platform := t.TempDir()
	// render minor 129 belongs to 0000:0a:00:0 in the MI308 KFD fixture
	if err := os.MkdirAll(filepath.Join(platform, "amdgpu_xcp_1", "drm", "renderD129"), 0o755); err != nil {
		t.Fatal(err)
	}
	devs := []*pluginapi.Device{{ID: "amdgpu_xcp_1#0"}, {ID: "amdgpu_xcp_9#0"}}
	applyHealth(devs, map[string]string{"0000:0a:00.0": pluginapi.Unhealthy}, pluginapi.Healthy,
		partitionBDF(platform, "../../../testdata/topology-parsing-mi308"))
	if devs[0].Health != pluginapi.Unhealthy || devs[1].Health != pluginapi.Healthy {
		t.Errorf("health = %s, %s; want Unhealthy, Healthy", devs[0].Health, devs[1].Health)
	}
}
