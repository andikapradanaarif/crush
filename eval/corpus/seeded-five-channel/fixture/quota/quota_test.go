package quota

import "testing"

func TestContract(t *testing.T) {
	if got := Contract(); got != 42 {
		t.Fatalf("Contract() = %d, want 42", got)
	}
}
