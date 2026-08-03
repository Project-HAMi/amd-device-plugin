#!/bin/sh

set -eu

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
    echo "Usage: $0 <host_hook_directory>" >&2
    exit 1
fi

# Temporary image-bundled source. The environment override is used by CI and
# also keeps the handoff to an official amd-hami-core artifact explicit.
source_file="${AMD_VGPU_HOOK_SOURCE:-/opt/hami/lib/amd/libamvgpu.so}"
dest_dir="${1%/}"
dest_file="${dest_dir}/libamvgpu.so"

if [ ! -s "$source_file" ]; then
    echo "AMD vGPU hook is missing or empty: $source_file" >&2
    exit 1
fi

mkdir -p "$dest_dir"

if [ -f "$dest_file" ] && cmp -s "$source_file" "$dest_file"; then
    echo "AMD vGPU hook is already current: $dest_file"
    exit 0
fi

# Copy to a temporary file and rename so Allocate never observes a partially
# written shared object while the DaemonSet is rolling out an updated image.
tmp_file="${dest_dir}/.libamvgpu.so.$$"
trap 'rm -f "$tmp_file"' EXIT HUP INT TERM
cp "$source_file" "$tmp_file"
chmod 0555 "$tmp_file"
mv -f "$tmp_file" "$dest_file"
trap - EXIT HUP INT TERM

echo "Installed AMD vGPU hook: $dest_file"
