package checkout

import (
	"testing"

	"shop/internal/cart"
	"shop/internal/catalog"
)

func TestRunShape(t *testing.T) {
	var c cart.Cart
	it, _ := catalog.Lookup("WIDGET")
	c.Add(it, 1)
	r := Run(c, "local")
	if r.LineCount != 1 {
		t.Fatalf("lines = %d, want 1", r.LineCount)
	}
	if r.ShippingCents < 0 {
		t.Fatal("negative shipping")
	}
}
