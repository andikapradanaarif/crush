package order

import (
	"testing"

	"example.com/bistro/internal/menu"
)

func TestTotal(t *testing.T) {
	m := &menu.Menu{}
	m.Add(menu.Dish{Name: "soto", Cents: 700})
	m.Add(menu.Dish{Name: "teh", Cents: 150})

	got, ok := Total(m, Order{Lines: []Line{
		{Dish: "soto", Qty: 2},
		{Dish: "teh", Qty: 1},
	}})
	if !ok || got != 1550 {
		t.Fatalf("Total = %d,%v; want 1550,true", got, ok)
	}
}

func TestTotalUnknownDish(t *testing.T) {
	m := &menu.Menu{}
	if _, ok := Total(m, Order{Lines: []Line{{Dish: "gado", Qty: 1}}}); ok {
		t.Fatal("unknown dish must fail the order")
	}
}
