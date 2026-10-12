package ledger

import "testing"

func TestBalance(t *testing.T) {
	Post("sale", 1200)
	Post("refund", -200)
	if Balance() < 0 {
		t.Fatal("negative balance")
	}
}
