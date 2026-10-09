package plugin

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Project-HAMi/amd-device-plugin/internal/pkg/utils"
)

func gpu(id string, index uint, healthy bool) *utils.DeviceInfo {
	return &utils.DeviceInfo{ID: id, Index: index, Health: healthy, CustomInfo: map[string]any{"pciBDF": "0000:0" + string(rune('0'+index)) + ":00.0"}}
}

func reasons(events []healthEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Reason)
	}
	return out
}

func TestHealthTransitions(t *testing.T) {
	healthy := []*utils.DeviceInfo{gpu("a", 0, true), gpu("b", 1, true)}

	// The first registration is a baseline: a GPU that is already sick is not news.
	events, state := healthTransitions(nil, []*utils.DeviceInfo{gpu("a", 0, true), gpu("b", 1, false)})
	if len(events) != 0 || state["b"].State != gpuUnhealthy {
		t.Fatalf("baseline = %v / %v", events, state)
	}

	steps := []struct {
		name    string
		now     []*utils.DeviceInfo
		reasons []string
	}{
		{"nothing changed", healthy, nil},
		{"a goes unhealthy", []*utils.DeviceInfo{gpu("a", 0, false), gpu("b", 1, true)}, []string{reasonUnhealthy}},
		{"still unhealthy says nothing", []*utils.DeviceInfo{gpu("a", 0, false), gpu("b", 1, true)}, nil},
		{"a recovers", healthy, []string{reasonRecovered}},
		{"b leaves the registration", []*utils.DeviceInfo{gpu("a", 0, true)}, []string{reasonMissing}},
		{"still missing says nothing", []*utils.DeviceInfo{gpu("a", 0, true)}, nil},
		{"b returns", healthy, []string{reasonRecovered}},
		{"b returns sick", []*utils.DeviceInfo{gpu("a", 0, true)}, []string{reasonMissing}},
		{"b back but unhealthy", []*utils.DeviceInfo{gpu("a", 0, true), gpu("b", 1, false)}, []string{reasonUnhealthy}},
		{"b healthy again", healthy, []string{reasonRecovered}},
	}
	state = map[string]gpuState{"a": {gpuHealthy, 0, "x"}, "b": {gpuHealthy, 1, "y"}}
	for _, s := range steps {
		var ev []healthEvent
		ev, state = healthTransitions(state, s.now)
		if got := reasons(ev); !reflect.DeepEqual(got, s.reasons) {
			t.Fatalf("%s: events %v, want %v", s.name, got, s.reasons)
		}
	}
}

func TestHealthTransitionsMessagesAndTypes(t *testing.T) {
	prev := map[string]gpuState{"uuid-1": {gpuHealthy, 3, "0000:0a:00.0"}}
	events, _ := healthTransitions(prev, nil)
	if len(events) != 1 || events[0].Type != corev1.EventTypeWarning || events[0].Reason != reasonMissing {
		t.Fatalf("events = %+v", events)
	}
	for _, want := range []string{"GPU 3", "0000:0a:00.0", "uuid-1"} {
		if !strings.Contains(events[0].Message, want) {
			t.Fatalf("message %q lacks %q", events[0].Message, want)
		}
	}
	events, _ = healthTransitions(map[string]gpuState{"u": {gpuUnhealthy, 0, "b"}}, []*utils.DeviceInfo{gpu("u", 0, true)})
	if len(events) != 1 || events[0].Type != corev1.EventTypeNormal {
		t.Fatalf("recovery = %+v", events)
	}
}
