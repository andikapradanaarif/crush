// Package returns computes refund amounts for sent-back items.
package returns

// Refund is the line's original cents reduced by the used share —
// restocking math, truncation toward the customer as with discount.
func Refund(lineTotalCents, pctUsed int) int {
	if pctUsed <= 0 {
		return lineTotalCents
	}
	if pctUsed >= 100 {
		return 0
	}
	return lineTotalCents * (100 - pctUsed) / 100
}
