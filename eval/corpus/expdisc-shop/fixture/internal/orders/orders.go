// Package orders assembles lines into a total. The per-line
// arithmetic is delegated to pricing — an order doesn't know how
// a line's cents are computed, only that they are.
package orders

import (
	"shop/internal/catalog"
	"shop/internal/pricing"
)

// Line is one order row.
type Line struct {
	Item catalog.Item
	Qty  int
}

// Order is the assembled purchase.
type Order struct {
	Lines []Line
}

// Total sums each line's priced total.
func Total(o Order) int {
	sum := 0
	for _, l := range o.Lines {
		sum += pricing.LineTotal(l.Qty, l.Item.PriceCents)
	}
	return sum
}
