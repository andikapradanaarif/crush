package invoice

import "testing"

func TestTotalRoundsUpStartedHour(t *testing.T) {
	if got := Total(61, 5000); got != 10000 {
		t.Fatalf("Total(61, 5000) = %d; want 10000", got)
	}
	if got := Total(60, 5000); got != 5000 {
		t.Fatalf("Total(60, 5000) = %d; want 5000", got)
	}
	if got := Total(0, 5000); got != 0 {
		t.Fatalf("Total(0, 5000) = %d; want 0", got)
	}
}
