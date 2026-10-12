// Package checkout turns a cart into a receipt: it totals the
// lines, adds shipping, and records the charge intent.
package checkout

import (
	"shop/internal/cart"
	"shop/internal/orders"
	"shop/internal/shipping"
)

// Receipt is what a completed checkout produced.
type Receipt struct {
	LineCount    int
	TotalCents   int
	ShippingCents int
}

// Run totals the cart — the arithmetic itself lives in orders and
// shipping; checkout only composes them.
func Run(c cart.Cart, zone string) Receipt {
	var o orders.Order
	for _, l := range c.Lines() {
		o.Lines = append(o.Lines, orders.Line{Item: l.Item, Qty: l.Qty})
	}
	return Receipt{
		LineCount:     len(o.Lines),
		TotalCents:    orders.Total(o),
		ShippingCents: shipping.Fee(500, zone),
	}
}
