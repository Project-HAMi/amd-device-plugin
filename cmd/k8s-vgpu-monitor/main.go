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

// Command k8s-vgpu-monitor serves Prometheus metrics for the AMD GPUs of one
// node: per-container VRAM from the dmem cgroup controller and host memory,
// load, temperature and power from sysfs, under the metric names of the HAMi
// NVIDIA vGPUmonitor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/vgpumonitor"
)

func main() {
	var bind, cgroupRoot, drmRoot string
	flag.StringVar(&bind, "metrics_bind_address", ":9394", "TCP address to serve /metrics on")
	flag.StringVar(&cgroupRoot, "cgroup_root", vgpumonitor.DefaultCgroupRoot, "cgroup v2 mount point of the host")
	flag.StringVar(&drmRoot, "drm_root", vgpumonitor.DefaultDRMRoot, "sysfs DRM class directory of the host")
	flag.Parse()

	if err := run(bind, cgroupRoot, drmRoot); err != nil {
		glog.Errorf("%v", err)
		glog.Flush()
		os.Exit(1)
	}
}

func run(bind, cgroupRoot, drmRoot string) error {
	nodeName := os.Getenv(utils.NodeNameEnvName)
	if nodeName == "" {
		return fmt.Errorf("env %s not set", utils.NodeNameEnvName)
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load the in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to build clientset: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Bind before the informer starts: a taken port never heals, so fail fast.
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", bind, err)
	}
	defer func() { _ = listener.Close() }()
	return serve(ctx, clientset, nodeName, listener, cgroupRoot, drmRoot)
}

// serve runs the collector for nodeName until ctx is done.
func serve(ctx context.Context, clientset kubernetes.Interface, nodeName string, listener net.Listener, cgroupRoot, drmRoot string) error {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 5*time.Minute,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = "spec.nodeName=" + nodeName }))
	podInformer := factory.Core().V1().Pods()
	podLister := podInformer.Lister()
	synced := podInformer.Informer().HasSynced
	factory.Start(ctx.Done())
	if !waitForSync(ctx, synced) {
		return errors.New("failed to sync pod informer cache")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(&vgpumonitor.Collector{
		NodeName:   nodeName,
		CgroupRoot: cgroupRoot,
		DRMRoot:    drmRoot,
		Pods: func() ([]*corev1.Pod, error) {
			return podLister.List(labels.Everything())
		},
		Node: func() (*corev1.Node, error) {
			return clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		},
	})
	glog.Infof("Serving AMD metrics for node %s on %s", nodeName, listener.Addr())
	return serveMetrics(ctx, listener, reg)
}

func waitForSync(ctx context.Context, synced func() bool) bool {
	for !synced() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return true
}

// serveMetrics serves reg on listener until ctx is done or the server fails.
func serveMetrics(ctx context.Context, listener net.Listener, reg *prometheus.Registry) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 60 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
