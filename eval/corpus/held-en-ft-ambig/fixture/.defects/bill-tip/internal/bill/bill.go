// Package bill computes line items and service charges.
package bill

// LineTotal returns a line's total in cents: qty × unit cents.
func LineTotal(qty, unitCents int) int {
	return qty * unitCents
}

// ServiceCharge returns the charge in cents for a percentage of the
// subtotal — subtotal*pct/100 with the product taken first so the
// division keeps its remainder.
func ServiceCharge(subtotalCents, pct int) int {
	return subtotalCents / 100 * pct
}
