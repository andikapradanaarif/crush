package tax

import "testing"

func TestTax(t *testing.T) {
	if got := Tax(1000, "standard"); got != 80 {
		t.Fatalf("Tax = %d, want 80", got)
	}
	if got := Tax(1000, "flat"); got != 0 {
		t.Fatalf("flat Tax = %d", got)
	}
}
