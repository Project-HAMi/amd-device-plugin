package plugin

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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

func TestModeChanges(t *testing.T) {
	soft := `[{"id":"a","mode":""},{"id":"b","mode":""}]`
	now := []*utils.DeviceInfo{{ID: "a", Mode: "spx"}, {ID: "b"}, {ID: "c", Mode: "spx"}}
	ids := func(ds []*utils.DeviceInfo) (out []string) {
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	for name, tc := range map[string]struct {
		previous string
		want     []string
	}{
		"first registration": {"", nil},
		"unreadable":         {"not json", nil},
		"cu to partition":    {soft, []string{"a"}},
		"unchanged":          {`[{"id":"a","mode":"spx"},{"id":"b","mode":""}]`, nil},
		"partition to cu":    {`[{"id":"b","mode":"cpx"}]`, []string{"b"}},
	} {
		if got := ids(modeChanges(tc.previous, now)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: changed = %v, want %v", name, got, tc.want)
		}
	}
}

func TestWarnModeChangeInUse(t *testing.T) {
	pod := func(name, alloc string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{utils.DeviceAllocation: alloc}},
			Spec:       corev1.PodSpec{NodeName: "gpu-node"},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node", Annotations: map[string]string{
		registerAnnosKey: `[{"id":"gpu-a","mode":""},{"id":"gpu-b","mode":""}]`,
	}}}
	old := utils.KubeClient
	defer func() { utils.KubeClient = old }()
	cs := fake.NewSimpleClientset(node,
		pod("on-a", "gpu-a,MI325X,1024,76:;", corev1.PodRunning),
		pod("done-on-a", "gpu-a,MI325X,1024,76:;", corev1.PodSucceeded),
		pod("on-b", "gpu-b,MI325X,1024,76:;", corev1.PodRunning),
	)
	utils.KubeClient = cs

	// gpu-a switched with a Pod on it, gpu-b kept its mode: one warning, for gpu-a only.
	warnModeChangeInUse(node, []*utils.DeviceInfo{{ID: "gpu-a", Index: 1, Mode: "spx"}, {ID: "gpu-b", Index: 2}})
	events, err := cs.CoreV1().Events(nodeEventNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("events = %+v, want one", events.Items)
	}
	ev := events.Items[0]
	if ev.Type != corev1.EventTypeWarning || ev.Reason != reasonModeChangedInUse ||
		!strings.Contains(ev.Message, "default/on-a") || strings.Contains(ev.Message, "done-on-a") || strings.Contains(ev.Message, "on-b") ||
		!strings.Contains(ev.Message, "partition (spx)") {
		t.Fatalf("event = %s %s %q", ev.Type, ev.Reason, ev.Message)
	}

	// The next registration sees its own annotation: nothing changed, no new warning.
	node.Annotations[registerAnnosKey] = `[{"id":"gpu-a","mode":"spx"},{"id":"gpu-b","mode":""}]`
	warnModeChangeInUse(node, []*utils.DeviceInfo{{ID: "gpu-a", Index: 1, Mode: "spx"}, {ID: "gpu-b", Index: 2}})
	if events, _ = cs.CoreV1().Events(nodeEventNamespace).List(context.Background(), metav1.ListOptions{}); len(events.Items) != 1 {
		t.Fatalf("repeat registration added events: %+v", events.Items)
	}
}
