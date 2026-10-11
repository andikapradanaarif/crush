package returns

import "testing"

func TestRefund(t *testing.T) {
	if got := Refund(1000, 25); got != 750 {
		t.Fatalf("Refund = %d", got)
	}
}
