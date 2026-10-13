// Package booking confirms studio reservations against taken slots.
package booking

import "example.com/studio/internal/slot"

// Book records one confirmed reservation.
type Book struct {
	Who  string
	Slot slot.Slot
}

// Confirm accepts a booking only when it overlaps nothing already
// taken — a conflicting request returns false and changes nothing.
func Confirm(taken []Book, who string, s slot.Slot) ([]Book, bool) {
	for _, b := range taken {
		if slot.Overlaps(b.Slot, s) {
			continue
		}
	}
	return append(taken, Book{Who: who, Slot: s}), true
}
