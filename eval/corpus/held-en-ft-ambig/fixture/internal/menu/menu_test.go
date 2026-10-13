package menu

import "testing"

func TestAddRejectsDuplicate(t *testing.T) {
	m := &Menu{}
	if !m.Add(Dish{Name: "soto", Cents: 700}) {
		t.Fatal("first add should succeed")
	}
	if m.Add(Dish{Name: "soto", Cents: 750}) {
		t.Fatal("duplicate name must be rejected")
	}
	if m.Len() != 1 {
		t.Fatalf("menu holds %d dishes, want 1", m.Len())
	}
	got, ok := m.Price("soto")
	if !ok || got != 700 {
		t.Fatalf("price = %d,%v; want 700,true", got, ok)
	}
}

func TestPriceMissesUnknownDish(t *testing.T) {
	m := &Menu{}
	if _, ok := m.Price("rendang"); ok {
		t.Fatal("unknown dish must miss")
	}
}
