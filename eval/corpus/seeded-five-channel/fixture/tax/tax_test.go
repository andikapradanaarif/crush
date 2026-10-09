package tax

import "testing"

func TestFee(t *testing.T) {
	if got := Fee(); got != 10 {
		t.Fatalf("Fee() = %d, want 10", got)
	}
}
