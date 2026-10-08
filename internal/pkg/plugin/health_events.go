package plugin

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
)

const (
	eventSource        = "amd-device-plugin"
	gpuHealthy         = "healthy"
	gpuUnhealthy       = "unhealthy"
	gpuMissing         = "missing"
	reasonUnhealthy    = "AMDGPUUnhealthy"
	reasonMissing      = "AMDGPUMissing"
	reasonRecovered    = "AMDGPURecovered"
	nodeEventNamespace = metav1.NamespaceDefault
)

// gpuState is what the plugin last saw of one registered GPU.
type gpuState struct {
	State string
	Index uint
	BDF   string
}

type healthEvent struct {
	Type, Reason, Message string
}

// healthTransitions compares the GPUs now registered with the states seen
// before and returns the events for every change, plus the new states. The first
// call (previous == nil) only records the baseline, so a plugin restart does not
// report GPUs that were already sick.
func healthTransitions(previous map[string]gpuState, current []*utils.DeviceInfo) ([]healthEvent, map[string]gpuState) {
	next := make(map[string]gpuState, len(current))
	for _, d := range current {
		bdf, _ := d.CustomInfo["pciBDF"].(string)
		state := gpuUnhealthy
		if d.Health {
			state = gpuHealthy
		}
		next[d.ID] = gpuState{State: state, Index: d.Index, BDF: bdf}
	}
	if previous == nil {
		return nil, next
	}
	var events []healthEvent
	ids := make([]string, 0, len(previous)+len(next))
	for id := range previous {
		ids = append(ids, id)
	}
	for id := range next {
		if _, seen := previous[id]; !seen {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		before, was := previous[id]
		now, is := next[id]
		switch {
		case !is && was && before.State != gpuMissing:
			// The GPU left the registration, for instance because its DRM device cannot be opened.
			next[id] = gpuState{State: gpuMissing, Index: before.Index, BDF: before.BDF}
			events = append(events, healthEvent{corev1.EventTypeWarning, reasonMissing,
				fmt.Sprintf("GPU %d (%s, %s) is no longer registered, no pod can be placed on it", before.Index, before.BDF, id)})
		case !is && was:
			// Still missing: keep remembering it so its return is reported.
			next[id] = before
		case is && now.State == gpuUnhealthy && (!was || before.State != gpuUnhealthy):
			events = append(events, healthEvent{corev1.EventTypeWarning, reasonUnhealthy,
				fmt.Sprintf("GPU %d (%s, %s) is unhealthy, the scheduler stops placing pods on it", now.Index, now.BDF, id)})
		case is && now.State == gpuHealthy && was && before.State != gpuHealthy:
			events = append(events, healthEvent{corev1.EventTypeNormal, reasonRecovered,
				fmt.Sprintf("GPU %d (%s, %s) is healthy again", now.Index, now.BDF, id)})
		}
	}
	return events, next
}

// emitNodeEvent records an Event on the node. It is best effort: a failure is
// logged and never fails the registration.
func emitNodeEvent(node *corev1.Node, ev healthEvent) {
	now := metav1.NewTime(time.Now())
	_, err := utils.GetClient().CoreV1().Events(nodeEventNamespace).Create(context.Background(), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: node.Name + ".", Namespace: nodeEventNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node", Name: node.Name, UID: node.UID, APIVersion: "v1",
		},
		Reason: ev.Reason, Message: ev.Message, Type: ev.Type,
		Source:         corev1.EventSource{Component: eventSource, Host: node.Name},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}, metav1.CreateOptions{})
	if err != nil {
		glog.Warningf("record node event %s: %v", ev.Reason, err)
	}
}
