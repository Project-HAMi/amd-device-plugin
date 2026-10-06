package exporter

import (
	"errors"
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

// A stale socket from a stopped exporter fails every health check; only the
// transitions may be logged at warning level.
func TestLogExporterStateLogsTransitionsOnce(t *testing.T) {
	exporterDown.Store(false)
	warnings := 0
	old := warnf
	warnf = func(string, ...any) { warnings++ }
	t.Cleanup(func() { exporterDown.Store(false); warnf = old })
	down := errors.New("connection refused")
	for i, step := range []struct {
		err       error
		wantDown  bool
		wantState bool
	}{
		{down, true, true},
		{down, true, true},
		{nil, false, false},
		{down, true, true},
	} {
		if got := logExporterState(step.err); got != step.wantDown {
			t.Errorf("step %d: logExporterState = %v, want %v", i, got, step.wantDown)
		}
		if exporterDown.Load() != step.wantState {
			t.Errorf("step %d: exporterDown = %v, want %v", i, exporterDown.Load(), step.wantState)
		}
	}
	// down, still down, back, down again: one warning per outage
	if warnings != 2 {
		t.Errorf("warnings = %d, want 2", warnings)
	}
}

// A stale socket fails every query; NewClient succeeding without dialing must
// not count as the exporter coming back, or each check warns again.
func TestStaleSocketWarnsOnce(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "exporter.socket")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldSock, oldWarn := healthSocket, warnf
	warnings := 0
	healthSocket, warnf = sock, func(string, ...any) { warnings++ }
	exporterDown.Store(false)
	t.Cleanup(func() { healthSocket, warnf = oldSock, oldWarn; exporterDown.Store(false) })
	for range 3 {
		if _, err := getGPUHealth(); err == nil {
			t.Fatal("query over a stale socket succeeded")
		}
	}
	if warnings != 1 {
		t.Errorf("warnings = %d over 3 failed checks, want 1", warnings)
	}
}
