package catalog

import "testing"

func TestLookupWidget(t *testing.T) {
	it, ok := Lookup("WIDGET")
	if !ok {
		t.Fatal("WIDGET missing")
	}
	if it.PriceCents != 595 {
		t.Fatalf("price = %d, want 595", it.PriceCents)
	}
	if _, ok := Lookup("NOPE"); ok {
		t.Fatal("unknown SKU resolved")
	}
}
