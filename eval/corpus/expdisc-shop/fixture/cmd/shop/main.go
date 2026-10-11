package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"shop/internal/catalog"
	"shop/internal/currency"
	"shop/internal/orders"
)

// shop total <orderfile> — the order file is one "SKU QTY" pair per
// line. The CLI is the end-to-end surface the orders total flows
// through; it intentionally contains no arithmetic of its own.
func main() {
	if len(os.Args) < 3 || os.Args[1] != "total" {
		fmt.Fprintln(os.Stderr, "usage: shop total <orderfile>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var o orders.Order
	for ln, row := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(row)
		if len(f) != 2 {
			fmt.Fprintf(os.Stderr, "line %d: want SKU QTY\n", ln+1)
			os.Exit(1)
		}
		qty, err := strconv.Atoi(f[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: %v\n", ln+1, err)
			os.Exit(1)
		}
		item, ok := catalog.Lookup(f[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "line %d: unknown sku %q\n", ln+1, f[0])
			os.Exit(1)
		}
		o.Lines = append(o.Lines, orders.Line{Item: item, Qty: qty})
	}
	fmt.Printf("total: %s\n", currency.Format(orders.Total(o)))
}
