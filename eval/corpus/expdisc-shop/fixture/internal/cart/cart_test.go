package cart

import (
	"testing"

	"shop/internal/catalog"
)

func TestAddAndClear(t *testing.T) {
	var c Cart
	it, _ := catalog.Lookup("BOLT")
	c.Add(it, 4)
	c.Add(it, 2)
	if len(c.Lines()) != 2 {
		t.Fatalf("lines = %d, want 2", len(c.Lines()))
	}
	c.Clear()
	if len(c.Lines()) != 0 {
		t.Fatal("clear left lines")
	}
}
