package ledger

import "testing"

func TestBalance(t *testing.T) {
	l := &Ledger{}
	l.Add(Entry{Memo: "print", Cents: 1200})
	l.Add(Entry{Memo: "ship", Cents: -300})
	if got := l.Balance(); got != 900 {
		t.Fatalf("Balance = %d; want 900", got)
	}
}
