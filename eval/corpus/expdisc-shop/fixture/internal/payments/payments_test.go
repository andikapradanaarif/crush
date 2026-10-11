package payments

import "testing"

func TestCharge(t *testing.T) {
	id, err := Charge(500, "tok")
	if err != nil || id == "" {
		t.Fatalf("Charge = %q, %v", id, err)
	}
	if _, err := Charge(0, "tok"); err == nil {
		t.Fatal("zero amount charged")
	}
}
