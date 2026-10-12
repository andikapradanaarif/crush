// Package warehouse maps SKUs to shelf locations.
package warehouse

var bins = map[string]string{
	"WIDGET":   "A-01-03",
	"GADGET":   "A-02-01",
	"SPROCKET": "B-04-02",
	"BOLT":     "C-10-11",
	"WHEEL":    "B-01-09",
}

// Bin returns the SKU's shelf address.
func Bin(code string) string { return bins[code] }
