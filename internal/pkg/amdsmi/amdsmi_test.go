package amdsmi

import (
	"fmt"
	"testing"
)

func TestNormalizeBDF(t *testing.T) {
	for _, input := range []string{" 0000:83:00.0 ", "0000:83:00:0"} {
		if got, want := normalizeBDF(input), "0000:83:00.0"; got != want {
			t.Fatalf("normalizeBDF(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAMDSMICacheReturnsOnlyRequestedOnError(t *testing.T) {
	c := newAMDSCache(func(bdfs []string) (map[string]string, error) {
		return map[string]string{"a": "1"}, fmt.Errorf("b failed")
	})
	c.Get([]string{"a", "b"})
	got, err := c.Get([]string{"c"})
	if err == nil || len(got) != 0 {
		t.Fatalf("Get(c) = %v, %v; want empty map and an error", got, err)
	}
}
