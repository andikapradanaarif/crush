// Package cart holds the in-session basket. It stores references
// to catalog items; nothing here prices anything.
package cart

import "shop/internal/catalog"

// Line is one basket entry.
type Line struct {
	Item catalog.Item
	Qty  int
}

// Cart is an ordered basket.
type Cart struct {
	lines []Line
}

// Add appends qty of item.
func (c *Cart) Add(it catalog.Item, qty int) {
	c.lines = append(c.lines, Line{Item: it, Qty: qty})
}

// Lines returns the basket contents.
func (c *Cart) Lines() []Line { return c.lines }

// Clear empties the basket.
func (c *Cart) Clear() { c.lines = nil }
