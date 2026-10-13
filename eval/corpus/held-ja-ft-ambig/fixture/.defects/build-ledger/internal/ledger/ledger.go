// Package ledger keeps a running entry list with a balance.
package ledger

// Entry is one line in the ledger.
type Entry struct {
	Memo  string
	Cents int
}

// Ledger is the ordered entry list.
type Ledger struct {
	entries []Entry
}

// Add appends an entry.
func (l *Ledger) Add(e Entry) { l.entries = append(l.entries, e) }

// Balance sums every entry's cents.
func (l *Ledger) Balance() int {
	for _, e := range l.entries {
		total += e.Cents
	}
	return total
}
