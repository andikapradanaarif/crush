package calc

import "testing"

func TestDivide(t *testing.T) {
	if got := Divide(10, 2); got != 5 {
		t.Fatalf("Divide(10, 2) = %d, want 5", got)
	}
}

func TestDivideByZero(t *testing.T) {
	if got := Divide(1, 0); got != 0 {
		t.Fatalf("Divide(1, 0) = %d, want 0", got)
	}
}
