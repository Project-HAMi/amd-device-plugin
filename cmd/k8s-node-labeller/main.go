package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/amdgpu"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	log                      = logf.Log.WithName("amdgpu-node-labeller")
	gitDescribe              string
	allLabelKeys             = []string{}
	allExperimentalLabelKeys = []string{}
)

const (
	experimentalAMDPrefix = "beta.amd.com"
	amdPrefix             = "amd.com"
)

func init() {
	initLabelLists()
}

func initLabelLists() {
	// pre-generate all the available node labeller labels
	// these 2 lists will be used to clean up old labels on the node
	for name := range labelGenerators {
		allLabelKeys = append(allLabelKeys, createLabelPrefix(name, false))
		allExperimentalLabelKeys = append(allExperimentalLabelKeys, createLabelPrefix(name, true))
	}
}

// removeOldNodeLabels deletes every label the labeller manages: the bare
// keys and every <key>.<suffix> variant (multi-entry, count and firmware keys),
// so labels from a previous GPU or firmware never linger.
func removeOldNodeLabels(node *corev1.Node) {
	if node == nil {
		return
	}
	for k := range node.Labels {
		if isManagedLabel(k) {
			delete(node.Labels, k)
		}
	}
}

func isManagedLabel(k string) bool {
	for _, keys := range [][]string{allLabelKeys, allExperimentalLabelKeys} {
		for _, p := range keys {
			if k == p || strings.HasPrefix(k, p+".") {
				return true
			}
		}
	}
	return false
}

// productNameMaxLen keeps beta.amd.com/gpu.product-name.<value> keys within the
// 63-character label name limit.
const productNameMaxLen = 63 - len("gpu.product-name.")

// sanitizeLabelValue maps s onto the Kubernetes label value charset
// ([A-Za-z0-9._-], at most maxLen chars, alphanumeric at both ends).
func sanitizeLabelValue(s string, maxLen int) string {
	s = strings.NewReplacer("(", "", ")", "").Replace(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if r < 0x80 && (r == '.' || r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '_'
	}, s)
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// parseDeviceID returns the hex device id from a sysfs device file.
func parseDeviceID(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "0x")
}

func createLabelPrefix(name string, experimental bool) string {
	var prefix string
	if experimental {
		prefix = experimentalAMDPrefix
	} else {
		prefix = amdPrefix
	}

	return fmt.Sprintf("%s/gpu.%s", prefix, name)
}

func createLabels(kind string, entries map[string]int) map[string]string {
	labels := make(map[string]string, len(entries))

	prefix := createLabelPrefix(kind, true)
	for k, v := range entries {
		labels[fmt.Sprintf("%s.%s", prefix, k)] = strconv.Itoa(v)
		if len(entries) == 1 {
			labels[prefix] = k
		}
	}

	prefix = createLabelPrefix(kind, false)
	for k, v := range entries {
		if len(entries) == 1 {
			labels[prefix] = k
		} else {
			labels[fmt.Sprintf("%s.%s", prefix, k)] = strconv.Itoa(v)
		}
	}

	return labels
}

var reSizeInBytes = regexp.MustCompile(`size_in_bytes\s(\d+)`)
var reSimdCount = regexp.MustCompile(`simd_count\s(\d+)`)
var reSimdPerCu = regexp.MustCompile(`simd_per_cu\s(\d+)`)
var reDrmRenderMinor = regexp.MustCompile(`drm_render_minor\s(\d+)`)

// topologyRoot holds the KFD topology nodes; tests point it at a fake tree.
var topologyRoot = "/sys/class/kfd/kfd/topology/nodes"

// gpuTopologyNodes returns, for each GPU, the KFD topology node directory
// whose drm_render_minor matches the GPU's render node.
func gpuTopologyNodes(gpus map[string]map[string]interface{}) []string {
	files, err := filepath.Glob(filepath.Join(topologyRoot, "*", "properties"))
	if err != nil || len(files) == 0 {
		log.Error(err, "Fail to glob topology properties", "root", topologyRoot)
		return nil
	}
	var dirs []string
	for _, gpu := range gpus {
		for _, file := range files {
			minor, err := amdgpu.ParseTopologyProperties(file, reDrmRenderMinor)
			if err == nil && matchesRenderD(minor, gpu) {
				dirs = append(dirs, filepath.Dir(file))
				break
			}
		}
	}
	return dirs
}

var labelGenerators = map[string]func(map[string]map[string]interface{}) map[string]string{
	"firmware": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}

		for _, v := range gpus {
			var featVersions map[string]uint32
			var fwVersions map[string]uint32

			featVersions, fwVersions, err := amdgpu.GetFirmwareVersions(fmt.Sprintf("card%d", v["card"]))
			if err != nil {
				log.Error(err, "Fail to get firmware versions")
				continue
			}

			for fw, ver := range featVersions {
				counts[fmt.Sprintf("%s.feat.%d", fw, ver)]++
			}
			for fw, ver := range fwVersions {
				counts[fmt.Sprintf("%s.fw.%d", fw, ver)]++
			}
		}

		pfx := createLabelPrefix("firmware", true)
		results := make(map[string]string, len(counts))
		for k, v := range counts {
			results[fmt.Sprintf("%s.%s", pfx, k)] = strconv.Itoa(v)
		}
		return results
	},
	"family": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}

		for _, v := range gpus {
			fid, err := amdgpu.GetCardFamilyName(fmt.Sprintf("card%d", v["card"]))
			if err != nil {
				log.Error(err, "Fail to get card family name.")
				continue
			}
			counts[fid]++
		}

		return createLabels("family", counts)
	},
	"driver-version": func(gpus map[string]map[string]interface{}) map[string]string {
		version := ""
		for _, v := range gpus {
			versionPath := fmt.Sprintf("/sys/class/drm/card%d/device/driver/module/version", v["card"])
			b, err := os.ReadFile(versionPath)
			if err != nil {
				log.Error(err, versionPath)
				continue
			}
			version = strings.TrimSpace(string(b))
			break
		}

		pfx := createLabelPrefix("driver-version", false)
		return map[string]string{pfx: version}
	},
	"driver-src-version": func(gpus map[string]map[string]interface{}) map[string]string {
		version := ""
		for _, v := range gpus {
			versionPath := fmt.Sprintf("/sys/class/drm/card%d/device/driver/module/srcversion", v["card"])
			b, err := os.ReadFile(versionPath)
			if err != nil {
				log.Error(err, versionPath)
				continue
			}
			version = strings.TrimSpace(string(b))
			break
		}

		pfx := createLabelPrefix("driver-src-version", false)
		return map[string]string{pfx: version}
	},
	"device-id": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}

		for _, v := range gpus {
			devidPath := fmt.Sprintf("/sys/class/drm/card%d/device/device", v["card"])
			b, err := os.ReadFile(devidPath)
			if err != nil {
				log.Error(err, devidPath)
				continue
			}
			devid := parseDeviceID(string(b))
			if devid == "" {
				continue
			}
			counts[devid]++
		}

		return createLabels("device-id", counts)
	},
	"product-name": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}

		for _, v := range gpus {
			prodnamePath := fmt.Sprintf("/sys/class/drm/card%d/device/product_name", v["card"])
			b, err := os.ReadFile(prodnamePath)
			if err != nil {
				log.Error(err, prodnamePath)
			}
			prodName := sanitizeLabelValue(string(b), productNameMaxLen)
			// if we are not able to get the product name from sysfs, try to read the value using libdrm
			if prodName == "" {
				prodName, err = amdgpu.GetCardProductName(fmt.Sprintf("card%d", v["card"]))
				if err != nil {
					log.Error(err, prodnamePath)
				} else {
					prodName = sanitizeLabelValue(prodName, productNameMaxLen)
				}
			}
			if prodName == "" {
				continue
			}
			counts[prodName]++
		}

		return createLabels("product-name", counts)
	},
	"vram": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}
		for _, dir := range gpuTopologyNodes(gpus) {
			vramPath := filepath.Join(dir, "mem_banks", "0", "properties")
			vSize, err := amdgpu.ParseTopologyProperties(vramPath, reSizeInBytes)
			if err != nil {
				log.Error(err, vramPath)
				continue
			}
			counts[fmt.Sprintf("%dG", int(math.Round(float64(vSize)/(1<<30))))]++
		}
		return createLabels("vram", counts)
	},
	"simd-count": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}
		for _, dir := range gpuTopologyNodes(gpus) {
			s, err := amdgpu.ParseTopologyProperties(filepath.Join(dir, "properties"), reSimdCount)
			if err != nil {
				log.Error(err, "Error parsing simd-count")
				continue
			}
			counts[strconv.FormatInt(s, 10)]++
		}
		return createLabels("simd-count", counts)
	},
	"cu-count": func(gpus map[string]map[string]interface{}) map[string]string {
		counts := map[string]int{}
		for _, dir := range gpuTopologyNodes(gpus) {
			props := filepath.Join(dir, "properties")
			s, err := amdgpu.ParseTopologyProperties(props, reSimdCount)
			if err != nil {
				log.Error(err, "Error parsing simd-count")
				continue
			}
			c, err := amdgpu.ParseTopologyProperties(props, reSimdPerCu)
			if err != nil || c == 0 {
				log.Error(err, fmt.Sprintf("Error parsing simd-per-cu %d", c))
				continue
			}
			counts[strconv.FormatInt(s/c, 10)]++
		}
		return createLabels("cu-count", counts)
	},
	"compute-memory-partition": func(gpus map[string]map[string]interface{}) map[string]string {
		partitionCountMap := amdgpu.UniquePartitionConfigCount(gpus)
		isHomogeneous := amdgpu.IsHomogeneous()
		if isHomogeneous {
			for partitionType, count := range partitionCountMap {
				if count > 0 {
					pfx := createLabelPrefix("compute-memory-partition", false)
					return map[string]string{pfx: partitionType}
				}
			}
		}
		return map[string]string{}
	},
	"compute-partitioning-supported": func(gpus map[string]map[string]interface{}) map[string]string {
		val := strconv.FormatBool(amdgpu.IsComputePartitionSupported())
		pfx := createLabelPrefix("compute-partitioning-supported", false)
		return map[string]string{pfx: val}
	},
	"memory-partitioning-supported": func(gpus map[string]map[string]interface{}) map[string]string {
		val := strconv.FormatBool(amdgpu.IsMemoryPartitionSupported())
		pfx := createLabelPrefix("memory-partitioning-supported", false)
		return map[string]string{pfx: val}
	},
}

// matchesRenderD reports whether a parsed DRM render minor belongs to the given GPU.
func matchesRenderD(minor int64, gpu map[string]interface{}) bool {
	renderD, ok := gpu["renderD"].(int)
	return ok && minor == int64(renderD)
}

var labelProperties = make(map[string]*bool, len(labelGenerators))

func generateLabels(lblProps map[string]*bool) map[string]string {
	results := make(map[string]string, len(labelGenerators))
	gpus := amdgpu.GetAMDGPUs()

	for l, f := range labelGenerators {
		if !*lblProps[l] {
			continue
		}

		for k, v := range f(gpus) {
			results[k] = v
		}
	}
	return results
}

// ownNodeName returns the node this labeller runs on. Without it no node
// event matches and the labeller would silently never label anything.
func ownNodeName() (string, error) {
	name := os.Getenv("DS_NODE_NAME")
	if name == "" {
		return "", fmt.Errorf("DS_NODE_NAME is not set; set it to spec.nodeName through the downward API")
	}
	return name, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "AMD GPU Node Labeller for Kubernetes\n")
		fmt.Fprintf(os.Stderr, "%s version %s\n", os.Args[0], gitDescribe)
		fmt.Fprintln(os.Stderr, "Usage:")
		flag.PrintDefaults()
	}

	for k := range labelGenerators {
		labelProperties[k] = flag.Bool(k, false, "Set this to label nodes with "+k+" properties")
	}

	flag.Parse()

	logf.SetLogger(zap.New())
	entryLog := log.WithName("entrypoint")

	// Setup a Manager
	entryLog.Info("setting up manager")
	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{
		// disable the metrics server
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		entryLog.Error(err, "unable to set up overall controller manager")
		os.Exit(1)
	}

	// Setup a new controller to reconcile Node labels
	entryLog.Info("Setting up controller")
	c, err := controller.New("amdgpu-node-labeller", mgr, controller.Options{
		Reconciler: &reconcileNodeLabels{client: mgr.GetClient(),
			log:    log.WithName("reconciler"),
			labels: generateLabels(labelProperties)},
	})
	if err != nil {
		entryLog.Error(err, "unable to set up individual controller")
		os.Exit(1)
	}

	// the labeller only handles events for the node it runs on
	hostname, err := ownNodeName()
	if err != nil {
		entryLog.Error(err, "unable to find this node")
		os.Exit(1)
	}

	pred := predicate.TypedFuncs[*corev1.Node]{
		// Create returns true if the Create event should be processed
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Node]) bool {
			return hostname == e.Object.GetName()
		},

		// Delete returns true if the Delete event should be processed
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Node]) bool {
			return false
		},

		// Update returns true if the Update event should be processed
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Node]) bool {
			return false
		},

		// Generic returns true if the Generic event should be processed
		GenericFunc: func(e event.TypedGenericEvent[*corev1.Node]) bool {
			return false
		},
	}

	// Watch Nodes and enqueue Nodes object key
	if err := c.Watch(source.Kind(mgr.GetCache(), &corev1.Node{}, &handler.TypedEnqueueRequestForObject[*corev1.Node]{}, &pred)); err != nil {
		entryLog.Error(err, "unable to watch Node")
		os.Exit(1)
	}

	entryLog.Info("starting manager")
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		entryLog.Error(err, "unable to run manager")
		os.Exit(1)
	}
}
