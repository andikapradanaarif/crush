// Package payments is a stub charge gateway — enough shape to
// look real in a dependency graph, no real I/O.
package payments

import "fmt"

var seq int

// Charge records a charge intent and returns its id.
func Charge(amountCents int, token string) (string, error) {
	if amountCents <= 0 {
		return "", fmt.Errorf("amount must be positive")
	}
	seq++
	return fmt.Sprintf("ch_%06d", seq), nil
}
