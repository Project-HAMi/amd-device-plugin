Captured from nodepool-gpu-7b3016af2911 (144.202.57.22), kernel 6.8.0-136-generic,
8x AMD Instinct MI355X (gfx950, device_id 0x75a3) in SPX/NPS1, kubelet
device-plugin daemonset on 2026-08-19.

Mirrors the /sys layout the plugin reads:

- sys/class/kfd/kfd/topology/nodes/*/properties: KFD topology, trimmed to the
  nodes/*/properties files the discovery code parses (drm_render_minor,
  location_id, unique_id).
- sys/module/amdgpu/drivers/pci:amdgpu/<bdf>/: one dir per whole GPU with the
  compute/memory partition state and the drm entry names (card, controlD,
  renderD placeholders).
- sys/devices/platform/amdgpu_xcp_*/: 56 XCD devices (7 per GPU), each with
  uevent, modalias and its drm card/renderD names. On this kernel the XCP
  render minors are absent from KFD topology, so discovery skips them.

Sibling fixtures with the same node's data:

- amdsmi-mi355x.json: AMD SMI UUIDs and market names for the 8 BDFs (UUIDs
  from the node register annotation, market names from amd-smi static).
- amdsmi-static-mi355x.json: raw `amd-smi static --json` output (asic serial,
  vram, clocks, RAS, xgmi links).
- amdsmi-partition-mi355x.json: raw `amd-smi partition --json` output
  (current memory/compute partition per GPU, partition profiles and resources).
- amdsmi-partition-m-mi355x.json: raw `amd-smi partition -m --json` output
  (per-GPU memory partition capabilities and current setting).
- amdsmi-partition-g0..g7-mi355x.json: raw `amd-smi partition -g <id> --json`
  output, one file per GPU. Identical except gpu_id: 4 profiles (SPX, DPX,
  QPX, CPX) with per-profile partition counts, XCC instances and other
  resource instances. amd-smi GPU IDs follow the sorted BDF order (ID 0 =
  0000:05:00.0 ... ID 7 = 0000:f5:00.0), same order the plugin sorts topology
  keys.
