// Package slot models booked time ranges in whole hours.
package slot

// Slot is a half-open range [Start, End) in hours.
type Slot struct {
	Start int
	End   int
}

// Overlaps reports whether two slots share any hour.
func Overlaps(a, b Slot) bool {
	return a.Start < b.End && b.Start < a.End
}
