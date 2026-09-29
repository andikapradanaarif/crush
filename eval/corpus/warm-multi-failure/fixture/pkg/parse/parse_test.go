package parse

import "testing"

func TestPortEmpty(t *testing.T) {
	if got := Port(""); got != 0 {
		t.Fatalf("Port(\"\") = %d, want 0", got)
	}
}

func TestPortValid(t *testing.T) {
	if got := Port("8080"); got != 8080 {
		t.Fatalf("Port(\"8080\") = %d, want 8080", got)
	}
}
