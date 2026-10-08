package tax

import "testing"

func TestFee(t *testing.T) {
	if Fee() != 10 {
		t.Fatalf("Fee() = %d, want 10", Fee())
	}
}
