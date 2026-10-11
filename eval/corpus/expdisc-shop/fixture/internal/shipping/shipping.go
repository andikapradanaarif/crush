// Package shipping prices fulfillment by weight band and zone.
package shipping

// Fee returns cents for shipping weightGrams to zone.
func Fee(weightGrams int, zone string) int {
	base := 499
	if zone == "remote" {
		base += 600
	}
	if weightGrams > 2000 {
		base += (weightGrams-2000+999)/1000 * 150
	}
	return base
}
