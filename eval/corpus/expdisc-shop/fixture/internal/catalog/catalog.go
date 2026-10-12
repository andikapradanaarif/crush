// Package catalog is the item/price lookup every order line
// resolves through. Prices are integer cents — the shop never
// floats money.
package catalog

// Item is one sellable thing.
type Item struct {
	Code       string
	Name       string
	PriceCents int
}

var items = map[string]Item{
	"WIDGET":   {Code: "WIDGET", Name: "Standard Widget", PriceCents: 595},
	"GADGET":   {Code: "GADGET", Name: "Pocket Gadget", PriceCents: 250},
	"SPROCKET": {Code: "SPROCKET", Name: "Steel Sprocket", PriceCents: 1125},
	"BOLT":     {Code: "BOLT", Name: "Hex Bolt M8", PriceCents: 35},
	"WHEEL":    {Code: "WHEEL", Name: "Caster Wheel", PriceCents: 1499},
}

// Lookup resolves a SKU to its item.
func Lookup(code string) (Item, bool) {
	it, ok := items[code]
	return it, ok
}
