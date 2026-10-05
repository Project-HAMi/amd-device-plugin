package exporter

import (
	"testing"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestApplyHealthMatchesSplitsByBDF(t *testing.T) {
	devs := []*pluginapi.Device{{ID: "0000:06:00.0#0"}, {ID: "0000:06:00.0#1"}, {ID: "0000:07:00.0#0"}}
	applyHealth(devs, map[string]string{"0000:06:00.0": pluginapi.Unhealthy}, pluginapi.Healthy)
	want := []string{pluginapi.Unhealthy, pluginapi.Unhealthy, pluginapi.Healthy}
	for i, d := range devs {
		if d.Health != want[i] {
			t.Errorf("%s: health %s, want %s", d.ID, d.Health, want[i])
		}
	}
}
