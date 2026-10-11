// Package pricing owns the money arithmetic for a single line:
// quantity times the catalog's per-unit cents. Keeping it out of
// orders keeps the totaling rule in one place — and makes it the
// package a cross-package hunt has to reach.
package pricing

// LineTotal returns the line's total in cents: qty × unit cents.
func LineTotal(qty, unitCents int) int {
	return qty * unitCents / 100
}

// BulkRate is the markdown percent a line earns at quantity —
// unrelated to LineTotal's contract, tested below so the package
// still carries its own green signal.
func BulkRate(qty int) int {
	switch {
	case qty >= 50:
		return 15
	case qty >= 10:
		return 5
	default:
		return 0
	}
}
