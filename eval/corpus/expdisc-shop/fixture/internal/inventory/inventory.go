// Package inventory tracks on-hand counts per SKU.
package inventory

import "fmt"

var stock = map[string]int{
	"WIDGET":   40,
	"GADGET":   60,
	"SPROCKET": 12,
	"BOLT":     500,
	"WHEEL":    25,
}

// Reserve decrements stock, erroring when the shelf is short.
func Reserve(code string, qty int) error {
	if stock[code] < qty {
		return fmt.Errorf("%s: %d on hand, want %d", code, stock[code], qty)
	}
	stock[code] -= qty
	return nil
}

// OnHand reports current stock.
func OnHand(code string) int { return stock[code] }
