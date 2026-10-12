package pricing

import "testing"

// BulkRate is the only rule this test pins — LineTotal is
// exercised downstream, in the orders suite, which is where a
// regression here surfaces first.
func TestBulkRate(t *testing.T) {
	if got := BulkRate(10); got != 5 {
		t.Fatalf("BulkRate = %d", got)
	}
	if got := BulkRate(60); got != 15 {
		t.Fatalf("BulkRate = %d", got)
	}
}
