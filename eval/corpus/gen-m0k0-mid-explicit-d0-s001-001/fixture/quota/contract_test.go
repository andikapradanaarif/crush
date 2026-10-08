package quota

import "testing"

func TestContract(t *testing.T) {
	if Contract() != 42 {
		t.Fatalf("Contract() = %d, want 42", Contract())
	}
}
