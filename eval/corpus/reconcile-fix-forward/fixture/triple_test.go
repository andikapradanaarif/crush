package fixforward

import "testing"

// TestTriple pins the contract Triple must satisfy.
func TestTriple(t *testing.T) {
	if got := Triple(3); got != 9 {
		t.Errorf("Triple(3) = %d; want 9", got)
	}
}
