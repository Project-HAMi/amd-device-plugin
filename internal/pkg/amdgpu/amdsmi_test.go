package amdgpu

import "testing"

func TestNormalizeBDF(t *testing.T) {
	for _, input := range []string{" 0000:83:00.0 ", "0000:83:00:0"} {
		if got, want := normalizeBDF(input), "0000:83:00.0"; got != want {
			t.Fatalf("normalizeBDF(%q) = %q, want %q", input, got, want)
		}
	}
}
