// Package invoice prices studio time by the started hour.
package invoice

// Total bills minutes at a per-hour rate in cents — any started
// hour counts as a full hour.
func Total(minutes, rateCents int) int {
	hours := (minutes + 59) / 60
	return hours * rateCents
}
