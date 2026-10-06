package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// cdiKind is the CDI vendor/class of the devices this plugin describes.
const cdiKind = "amd.com/gpu"

type cdiDeviceNode struct {
	Path string `json:"path"`
}

type cdiEdits struct {
	DeviceNodes []cdiDeviceNode `json:"deviceNodes"`
}

type cdiDevice struct {
	Name           string   `json:"name"`
	ContainerEdits cdiEdits `json:"containerEdits"`
}

type cdiSpec struct {
	Version        string      `json:"cdiVersion"`
	Kind           string      `json:"kind"`
	Devices        []cdiDevice `json:"devices"`
	ContainerEdits cdiEdits    `json:"containerEdits"`
}

// cdiDeviceName is the fully qualified CDI name of the GPU behind a DRM card.
func cdiDeviceName(card int) string {
	return fmt.Sprintf("%s=card%d", cdiKind, card)
}

// writeCDISpec writes a CDI spec with one device per GPU, named after its DRM
// card, plus /dev/kfd shared by all of them. The runtime resolves the device
// numbers from the host nodes.
func writeCDISpec(dir string, gpus map[string]map[string]interface{}) error {
	spec := cdiSpec{
		Version:        "0.6.0",
		Kind:           cdiKind,
		ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{{Path: "/dev/kfd"}}},
	}
	for id, g := range gpus {
		card, cok := g["card"].(int)
		renderD, rok := g["renderD"].(int)
		if !cok || !rok {
			return fmt.Errorf("GPU %s has no card or render node", id)
		}
		spec.Devices = append(spec.Devices, cdiDevice{
			Name: fmt.Sprintf("card%d", card),
			ContainerEdits: cdiEdits{DeviceNodes: []cdiDeviceNode{
				{Path: fmt.Sprintf("/dev/dri/card%d", card)},
				{Path: fmt.Sprintf("/dev/dri/renderD%d", renderD)},
			}},
		})
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// write and rename so a runtime never reads a partial spec
	tmp, err := os.CreateTemp(dir, ".amd.com-gpu-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "amd.com-gpu.json"))
}
