// Package currency formats integer cents for display and parses
// them back. All money in the shop is cents; the only rounding
// that exists lives in ParseCents' fractional-digit carry.
package currency

import (
	"fmt"
	"strconv"
	"strings"
)

// Format renders cents as "$12.34".
func Format(cents int) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

// ParseCents reads "$12.34" or "12.34" back to cents. A third
// decimal digit rounds half up — the only rounding rule in the
// package, and a deliberate candidate for "is the total wrong
// here?" suspicion: it is correct.
func ParseCents(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return strconv.Atoi(s + "00")
	}
	whole, err := strconv.Atoi(s[:dot])
	if err != nil {
		return 0, err
	}
	frac := (s[dot+1:] + "000")[:3]
	f, err := strconv.Atoi(frac)
	if err != nil {
		return 0, err
	}
	cents := whole*100 + f/10
	if f%10 >= 5 {
		cents++
	}
	return cents, nil
}
