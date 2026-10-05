package main

import (
	"sort"
	"strings"
	"testing"
)

func TestResourceNames(t *testing.T) {
	gpu := func(compute, memory string) map[string]interface{} {
		return map[string]interface{}{"computePartitionType": compute, "memoryPartitionType": memory}
	}
	rdna := map[string]map[string]interface{}{"a": gpu("", "")}
	spx := map[string]map[string]interface{}{"a": gpu("spx", "nps1"), "b": gpu("spx", "nps1")}
	// one busy GPU kept spx while the other flipped to qpx
	mixed := map[string]map[string]interface{}{"a": gpu("spx", "nps1"), "b": gpu("qpx", "nps1")}
	for _, tc := range []struct {
		name     string
		strategy ResourceNamingStrategy
		gpus     map[string]map[string]interface{}
		want     string
	}{
		{"no gpus", StrategySingle, nil, ""},
		{"rdna single", StrategySingle, rdna, "gpu"},
		{"rdna mixed", StrategyMixed, rdna, "gpu"},
		{"spx single", StrategySingle, spx, "gpu"},
		{"spx mixed", StrategyMixed, spx, "spx_nps1"},
		{"mixed node single", StrategySingle, mixed, "gpu"},
		{"mixed node mixed", StrategyMixed, mixed, "qpx_nps1,spx_nps1"},
	} {
		got := resourceNames(tc.strategy, tc.gpus)
		sort.Strings(got)
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%s: resources = %v, want %s", tc.name, got, tc.want)
		}
	}
}
