package inventory

import "testing"

func TestReserve(t *testing.T) {
	before := OnHand("BOLT")
	if err := Reserve("BOLT", 10); err != nil {
		t.Fatal(err)
	}
	if OnHand("BOLT") != before-10 {
		t.Fatal("reserve didn't decrement")
	}
	if err := Reserve("WHEEL", 9999); err == nil {
		t.Fatal("oversell allowed")
	}
}
