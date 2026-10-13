// Package order validates orders against the menu.
package order

import (
	"example.com/bistro/internal/bill"
	"example.com/bistro/internal/menu"
)

// Line is one ordered dish.
type Line struct {
	Dish string
	Qty  int
}

// Order is a table's request.
type Order struct {
	Lines []Line
}

// Total prices an order in cents; ok is false if a dish is unknown.
func Total(m *menu.Menu, o Order) (int, bool) {
	total := 0
	for _, l := range o.Lines {
		total += bill.LineTotal(l.Qty, m.Price(l.Dish))
	}
	return total, true
}
