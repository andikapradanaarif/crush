package bill

import "testing"

func TestLineTotal(t *testing.T) {
	if got := LineTotal(3, 450); got != 1350 {
		t.Fatalf("LineTotal(3, 450) = %d; want 1350", got)
	}
}

func TestServiceCharge(t *testing.T) {
	if got := ServiceCharge(999, 10); got != 99 {
		t.Fatalf("ServiceCharge(999, 10) = %d; want 99", got)
	}
	if got := ServiceCharge(250, 12); got != 30 {
		t.Fatalf("ServiceCharge(250, 12) = %d; want 30", got)
	}
}
