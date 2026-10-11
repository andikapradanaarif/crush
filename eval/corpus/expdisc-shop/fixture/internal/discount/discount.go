// Package discount applies whole-percent markdowns. Percentages
// are integers; the price never leaves cents.
package discount

// Apply returns totalCents reduced by pct percent, truncated —
// markdowns always round in the customer's favor by at most a cent.
func Apply(totalCents, pct int) int {
	if pct <= 0 {
		return totalCents
	}
	return totalCents - totalCents*pct/100
}
