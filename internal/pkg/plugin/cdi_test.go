package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCDISpec(t *testing.T) {
	dir := t.TempDir()
	gpus := map[string]map[string]interface{}{"0000:06:00.0": {"card": 1, "renderD": 129}}
	if err := writeCDISpec(dir, gpus); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "amd.com-gpu.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec cdiSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "amd.com/gpu" || len(spec.Devices) != 1 || spec.Devices[0].Name != "card1" {
		t.Fatalf("unexpected spec %+v", spec)
	}
	nodes := spec.Devices[0].ContainerEdits.DeviceNodes
	if len(nodes) != 2 || nodes[0].Path != "/dev/dri/card1" || nodes[1].Path != "/dev/dri/renderD129" {
		t.Errorf("device nodes %+v", nodes)
	}
	if got := spec.ContainerEdits.DeviceNodes; len(got) != 1 || got[0].Path != "/dev/kfd" {
		t.Errorf("shared nodes %+v", got)
	}
	if cdiDeviceName(1) != "amd.com/gpu=card1" {
		t.Errorf("cdiDeviceName(1) = %s", cdiDeviceName(1))
	}
	if err := writeCDISpec(dir, map[string]map[string]interface{}{"x": {}}); err == nil {
		t.Error("GPU without card should fail")
	}
}
