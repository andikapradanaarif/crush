package decoy

import "testing"

func TestValue(t *testing.T) {
	if got := Value(); got != 42 {
		t.Fatalf("Value() = %d, want 42", got)
	}
}
