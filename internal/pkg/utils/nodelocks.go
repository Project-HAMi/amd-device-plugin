/*
Copyright 2024 The HAMi Authors.

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

package utils

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	NodeLockKey = "hami.io/mutex.lock"
	NodeLockSep = ","
)

var (
	// nodeLocks manages per-node locks for fine-grained concurrency control.
	nodeLocks = newNodeLockManager()
	// NodeLockTimeout is the global timeout for node locks.
	NodeLockTimeout time.Duration = time.Minute * 5

	DefaultStrategy = wait.Backoff{
		Steps:    5,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.5,
	}
)

// nodeLockManager manages locks on a per-node basis to allow concurrent
// operations on different nodes while maintaining mutual exclusion for
// operations on the same node.
type nodeLockManager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// newNodeLockManager returns a nodeLockManager with its lock map initialized.
func newNodeLockManager() nodeLockManager {
	return nodeLockManager{
		locks: make(map[string]*sync.Mutex),
	}
}

// getLock returns the mutex for a specific node, creating it if necessary.
// This method is thread-safe.
func (m *nodeLockManager) getLock(nodeName string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.locks[nodeName]; !ok {
		m.locks[nodeName] = &sync.Mutex{}
	}
	return m.locks[nodeName]
}

func init() {
	setupNodeLockTimeout()
}

// setupNodeLockTimeout configures the node lock timeout from the environment.
func setupNodeLockTimeout() {
	nodelock := os.Getenv("HAMI_NODELOCK_EXPIRE")
	if nodelock != "" {
		d, err := time.ParseDuration(nodelock)
		if err != nil {
			klog.ErrorS(err, "Failed to parse HAMI_NODELOCK_EXPIRE, using default", "duration", NodeLockTimeout)
		} else {
			NodeLockTimeout = d
			klog.InfoS("Node lock expiration time set from environment variable", "duration", d)
		}
	}
}

func ReleaseNodeLock(nodeName string, lockname string, pod *corev1.Pod, skipNodeLockOwnerCheck bool) error {
	if pod == nil {
		return fmt.Errorf("cannot release node lock: pod is nil")
	}
	// Acquire per-node lock instead of global lock
	nodeLock := nodeLocks.getLock(nodeName)
	nodeLock.Lock()
	defer nodeLock.Unlock()

	ctx := context.Background()
	node, err := GetClient().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	lockStr, ok := node.Annotations[NodeLockKey]
	if !ok {
		return nil
	}
	if !skipNodeLockOwnerCheck && !strings.HasSuffix(lockStr, fmt.Sprintf("%s%s", NodeLockSep, GeneratePodNamespaceName(pod, NodeLockSep))) {
		klog.InfoS("NodeLock is not set by this pod", NodeLockKey, lockStr, "podName", pod.Name, "podNamespace", pod.Namespace)
		return nil
	}

	err = retry.OnError(DefaultStrategy, func(err error) bool {
		// Retry on any error
		return true
	}, func() error {
		node, err = GetClient().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to get node when retry to patch", "node", nodeName)
			return err
		}
		patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null},"resourceVersion":"%s"}}`, NodeLockKey, node.ResourceVersion)
		_, err = GetClient().CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patchData), metav1.PatchOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to patch node when retry to patch", "node", nodeName)
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to release node lock (node=%s, retry strategy=%+v): %w", nodeName, DefaultStrategy, err)
	}

	klog.InfoS("Node lock released", "node", nodeName, "podName", pod.Name)
	return nil
}

func ParseNodeLock(value string) (lockTime time.Time, ns, name string, err error) {
	if !strings.Contains(value, NodeLockSep) {
		lockTime, err = time.Parse(time.RFC3339, value)
		return lockTime, "", "", err
	}
	s := strings.Split(value, NodeLockSep)
	if len(s) != 3 {
		return time.Time{}, "", "", fmt.Errorf("malformed lock annotation: expected 3 parts, got %d from %s", len(s), value)
	}
	lockTime, err = time.Parse(time.RFC3339, s[0])
	return lockTime, s[1], s[2], err
}

func GeneratePodNamespaceName(pod *corev1.Pod, sep string) string {
	if pod == nil {
		return ""
	}
	return fmt.Sprintf("%s%s%s", pod.Namespace, sep, pod.Name)
}
