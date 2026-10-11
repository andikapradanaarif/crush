package orders

import (
	"testing"

	"shop/internal/catalog"
)

func TestOrderTotal(t *testing.T) {
	widget, _ := catalog.Lookup("WIDGET") // 595c
	gadget, _ := catalog.Lookup("GADGET") // 250c
	o := Order{Lines: []Line{
		{Item: widget, Qty: 2}, // 1190
		{Item: gadget, Qty: 1}, //  250
	}}
	if got := Total(o); got != 1440 {
		t.Fatalf("Total = %d, want 1440", got)
	}
}
