// Package metricssvc is the gRPC client for the AMD Device Metrics Exporter
// health socket. The .pb.go files are generated code copied unchanged from
// github.com/ROCm/device-metrics-exporter pkg/exporter/gen/metricssvc
// (commit 604c5281a8b0); update them by copying from there again. The plugin
// only calls List; the SetError RPC upstream added later is not needed.
package metricssvc
