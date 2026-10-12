package shipping

import "testing"

func TestFee(t *testing.T) {
	if got := Fee(500, "local"); got != 499 {
		t.Fatalf("Fee = %d", got)
	}
	if got := Fee(500, "remote"); got != 1099 {
		t.Fatalf("remote Fee = %d", got)
	}
}
