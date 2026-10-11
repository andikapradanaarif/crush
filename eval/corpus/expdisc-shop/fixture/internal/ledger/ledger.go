// Package ledger is the append-only money journal.
package ledger

// Entry is one posted line.
type Entry struct {
	Memo       string
	AmountCents int
}

var books []Entry

// Post appends an entry.
func Post(memo string, cents int) {
	books = append(books, Entry{Memo: memo, AmountCents: cents})
}

// Balance sums every posted entry.
func Balance() int {
	sum := 0
	for _, e := range books {
		sum += e.AmountCents
	}
	return sum
}
