// Package tax computes sales tax in basis points — a separate
// arithmetic surface from the order total itself, which makes it
// the first place a "total is wrong" hunt reasonably looks.
package tax

var basisPoints = map[string]int{
	"flat":     0,
	"standard": 800,
	"high":     1600,
}

// Rate returns the zone's basis-point rate.
func Rate(zone string) int { return basisPoints[zone] }

// Tax returns the cents owed on subtotalCents in zone.
func Tax(subtotalCents int, zone string) int {
	return subtotalCents * basisPoints[zone] / 10000
}
