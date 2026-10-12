package discount

import "testing"

func TestApply(t *testing.T) {
	if got := Apply(1000, 10); got != 900 {
		t.Fatalf("Apply = %d, want 900", got)
	}
	if got := Apply(1000, 0); got != 1000 {
		t.Fatalf("zero pct = %d", got)
	}
}
