//go:build private

package quota

import "testing"

func TestSampler(t *testing.T) {
	if Contract() != 99 {
		t.Fatalf("Contract() = %d, want 99", Contract())
	}
}
