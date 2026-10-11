#!/usr/bin/env python3
"""Emit the expdisc-shop fixture deterministically (#227).

The fixture is a ~18-package Go module whose suite is fully green in
the pristine state. Two per-family defects live under .defects/ —
each trajectory's setup overlays one and deletes the directory, so
the answer never sits in the measured workdir:

  .defects/ft/internal/pricing/pricing.go
      LineTotal truncates by /100 — the orders package test is the
      only failing signal; the buggy line is one package hop away.
  .defects/bb/internal/reports/report_test.go
      Calls Generate with a stale two-arg signature — `go build`
      stays clean because the break is in the test surface.

Regenerate with:  python3 genfixture.py
"""

import os
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent / "fixture"


def w(rel: str, content: str) -> None:
    p = ROOT / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(content)


w("go.mod", "module shop\n\ngo 1.21\n")

# --- cmd -----------------------------------------------------------------

w("cmd/shop/main.go", '''package main

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
	for ln, row := range strings.Split(strings.TrimSpace(string(data)), "\\n") {
		f := strings.Fields(row)
		if len(f) != 2 {
			fmt.Fprintf(os.Stderr, "line %d: want SKU QTY\\n", ln+1)
			os.Exit(1)
		}
		qty, err := strconv.Atoi(f[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: %v\\n", ln+1, err)
			os.Exit(1)
		}
		item, ok := catalog.Lookup(f[0])
		if !ok {
			fmt.Fprintf(os.Stderr, "line %d: unknown sku %q\\n", ln+1, f[0])
			os.Exit(1)
		}
		o.Lines = append(o.Lines, orders.Line{Item: item, Qty: qty})
	}
	fmt.Printf("total: %s\\n", currency.Format(orders.Total(o)))
}
''')

# --- leaf packages, each with one passing test ----------------------------

w("internal/catalog/catalog.go", '''// Package catalog is the item/price lookup every order line
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
''')

w("internal/catalog/catalog_test.go", '''package catalog

import "testing"

func TestLookupWidget(t *testing.T) {
	it, ok := Lookup("WIDGET")
	if !ok {
		t.Fatal("WIDGET missing")
	}
	if it.PriceCents != 595 {
		t.Fatalf("price = %d, want 595", it.PriceCents)
	}
	if _, ok := Lookup("NOPE"); ok {
		t.Fatal("unknown SKU resolved")
	}
}
''')

w("internal/cart/cart.go", '''// Package cart holds the in-session basket. It stores references
// to catalog items; nothing here prices anything.
package cart

import "shop/internal/catalog"

// Line is one basket entry.
type Line struct {
	Item catalog.Item
	Qty  int
}

// Cart is an ordered basket.
type Cart struct {
	lines []Line
}

// Add appends qty of item.
func (c *Cart) Add(it catalog.Item, qty int) {
	c.lines = append(c.lines, Line{Item: it, Qty: qty})
}

// Lines returns the basket contents.
func (c *Cart) Lines() []Line { return c.lines }

// Clear empties the basket.
func (c *Cart) Clear() { c.lines = nil }
''')

w("internal/cart/cart_test.go", '''package cart

import (
	"testing"

	"shop/internal/catalog"
)

func TestAddAndClear(t *testing.T) {
	var c Cart
	it, _ := catalog.Lookup("BOLT")
	c.Add(it, 4)
	c.Add(it, 2)
	if len(c.Lines()) != 2 {
		t.Fatalf("lines = %d, want 2", len(c.Lines()))
	}
	c.Clear()
	if len(c.Lines()) != 0 {
		t.Fatal("clear left lines")
	}
}
''')

w("internal/checkout/checkout.go", '''// Package checkout turns a cart into a receipt: it totals the
// lines, adds shipping, and records the charge intent.
package checkout

import (
	"shop/internal/cart"
	"shop/internal/orders"
	"shop/internal/shipping"
)

// Receipt is what a completed checkout produced.
type Receipt struct {
	LineCount    int
	TotalCents   int
	ShippingCents int
}

// Run totals the cart — the arithmetic itself lives in orders and
// shipping; checkout only composes them.
func Run(c cart.Cart, zone string) Receipt {
	var o orders.Order
	for _, l := range c.Lines() {
		o.Lines = append(o.Lines, orders.Line{Item: l.Item, Qty: l.Qty})
	}
	return Receipt{
		LineCount:     len(o.Lines),
		TotalCents:    orders.Total(o),
		ShippingCents: shipping.Fee(500, zone),
	}
}
''')

w("internal/checkout/checkout_test.go", '''package checkout

import (
	"testing"

	"shop/internal/cart"
	"shop/internal/catalog"
)

func TestRunShape(t *testing.T) {
	var c cart.Cart
	it, _ := catalog.Lookup("WIDGET")
	c.Add(it, 1)
	r := Run(c, "local")
	if r.LineCount != 1 {
		t.Fatalf("lines = %d, want 1", r.LineCount)
	}
	if r.ShippingCents < 0 {
		t.Fatal("negative shipping")
	}
}
''')

w("internal/currency/currency.go", '''// Package currency formats integer cents for display and parses
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
''')

w("internal/currency/currency_test.go", '''package currency

import "testing"

func TestFormatAndParse(t *testing.T) {
	if got := Format(1234); got != "$12.34" {
		t.Fatalf("Format = %q", got)
	}
	c, err := ParseCents("$12.34")
	if err != nil || c != 1234 {
		t.Fatalf("ParseCents = %d, %v", c, err)
	}
}
''')

w("internal/discount/discount.go", '''// Package discount applies whole-percent markdowns. Percentages
// are integers; the price never leaves cents.
package discount

// Apply returns totalCents reduced by pct percent, truncated —
// markdowns always round in the customer's favor by at most a cent.
func Apply(totalCents, pct int) int {
	if pct <= 0 {
		return totalCents
	}
	return totalCents - totalCents*pct/100
}
''')

w("internal/discount/discount_test.go", '''package discount

import "testing"

func TestApply(t *testing.T) {
	if got := Apply(1000, 10); got != 900 {
		t.Fatalf("Apply = %d, want 900", got)
	}
	if got := Apply(1000, 0); got != 1000 {
		t.Fatalf("zero pct = %d", got)
	}
}
''')

w("internal/inventory/inventory.go", '''// Package inventory tracks on-hand counts per SKU.
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
''')

w("internal/inventory/inventory_test.go", '''package inventory

import "testing"

func TestReserve(t *testing.T) {
	before := OnHand("BOLT")
	if err := Reserve("BOLT", 10); err != nil {
		t.Fatal(err)
	}
	if OnHand("BOLT") != before-10 {
		t.Fatal("reserve didn't decrement")
	}
	if err := Reserve("WHEEL", 9999); err == nil {
		t.Fatal("oversell allowed")
	}
}
''')

w("internal/ledger/ledger.go", '''// Package ledger is the append-only money journal.
package ledger

// Entry is one posted line.
type Entry struct {
	Memo       string
	AmountCents int
}

var books []Entry

// Post appends an entry.
func Post(memo string, cents int) {
	books = append(books, Entry{Memo: memo, AmountCents: cents})
}

// Balance sums every posted entry.
func Balance() int {
	sum := 0
	for _, e := range books {
		sum += e.AmountCents
	}
	return sum
}
''')

w("internal/ledger/ledger_test.go", '''package ledger

import "testing"

func TestBalance(t *testing.T) {
	Post("sale", 1200)
	Post("refund", -200)
	if Balance() < 0 {
		t.Fatal("negative balance")
	}
}
''')

w("internal/notify/notify.go", '''// Package notify queues outbound customer messages; the transport
// is out of scope for the eval fixture.
package notify

var outbox []string

// Send queues a message.
func Send(user, msg string) {
	outbox = append(outbox, user+": "+msg)
}

// Pending reports the queue depth.
func Pending() int { return len(outbox) }
''')

w("internal/notify/notify_test.go", '''package notify

import "testing"

func TestSendQueues(t *testing.T) {
	before := Pending()
	Send("u@x", "hi")
	if Pending() != before+1 {
		t.Fatal("send didn't queue")
	}
}
''')

w("internal/payments/payments.go", '''// Package payments is a stub charge gateway — enough shape to
// look real in a dependency graph, no real I/O.
package payments

import "fmt"

var seq int

// Charge records a charge intent and returns its id.
func Charge(amountCents int, token string) (string, error) {
	if amountCents <= 0 {
		return "", fmt.Errorf("amount must be positive")
	}
	seq++
	return fmt.Sprintf("ch_%06d", seq), nil
}
''')

w("internal/payments/payments_test.go", '''package payments

import "testing"

func TestCharge(t *testing.T) {
	id, err := Charge(500, "tok")
	if err != nil || id == "" {
		t.Fatalf("Charge = %q, %v", id, err)
	}
	if _, err := Charge(0, "tok"); err == nil {
		t.Fatal("zero amount charged")
	}
}
''')

w("internal/returns/returns.go", '''// Package returns computes refund amounts for sent-back items.
package returns

// Refund is the line's original cents reduced by the used share —
// restocking math, truncation toward the customer as with discount.
func Refund(lineTotalCents, pctUsed int) int {
	if pctUsed <= 0 {
		return lineTotalCents
	}
	if pctUsed >= 100 {
		return 0
	}
	return lineTotalCents * (100 - pctUsed) / 100
}
''')

w("internal/returns/returns_test.go", '''package returns

import "testing"

func TestRefund(t *testing.T) {
	if got := Refund(1000, 25); got != 750 {
		t.Fatalf("Refund = %d", got)
	}
}
''')

w("internal/sessions/sessions.go", '''// Package sessions is the in-memory token store for the CLI's
// single-user mode.
package sessions

var tokens = map[string]string{}

// Open issues a token for user.
func Open(user, token string) { tokens[token] = user }

// Who resolves a token.
func Who(token string) (string, bool) {
	u, ok := tokens[token]
	return u, ok
}
''')

w("internal/sessions/sessions_test.go", '''package sessions

import "testing"

func TestRoundTrip(t *testing.T) {
	Open("amy", "t1")
	if u, ok := Who("t1"); !ok || u != "amy" {
		t.Fatal("token didn't resolve")
	}
}
''')

w("internal/shipping/shipping.go", '''// Package shipping prices fulfillment by weight band and zone.
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
''')

w("internal/shipping/shipping_test.go", '''package shipping

import "testing"

func TestFee(t *testing.T) {
	if got := Fee(500, "local"); got != 499 {
		t.Fatalf("Fee = %d", got)
	}
	if got := Fee(500, "remote"); got != 1099 {
		t.Fatalf("remote Fee = %d", got)
	}
}
''')

w("internal/tax/tax.go", '''// Package tax computes sales tax in basis points — a separate
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
''')

w("internal/tax/tax_test.go", '''package tax

import "testing"

func TestTax(t *testing.T) {
	if got := Tax(1000, "standard"); got != 80 {
		t.Fatalf("Tax = %d, want 80", got)
	}
	if got := Tax(1000, "flat"); got != 0 {
		t.Fatalf("flat Tax = %d", got)
	}
}
''')

w("internal/users/users.go", '''// Package users is the customer record store.
package users

// User is a registered customer.
type User struct {
	ID    string
	Email string
	Zone  string
}

var byID = map[string]User{
	"u1": {ID: "u1", Email: "amy@shop.test", Zone: "standard"},
	"u2": {ID: "u2", Email: "ben@shop.test", Zone: "flat"},
}

// ByID resolves a user.
func ByID(id string) (User, bool) {
	u, ok := byID[id]
	return u, ok
}
''')

w("internal/users/users_test.go", '''package users

import "testing"

func TestByID(t *testing.T) {
	if u, ok := ByID("u1"); !ok || u.Zone != "standard" {
		t.Fatal("u1 lookup wrong")
	}
}
''')

w("internal/warehouse/warehouse.go", '''// Package warehouse maps SKUs to shelf locations.
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
''')

w("internal/warehouse/warehouse_test.go", '''package warehouse

import "testing"

func TestBin(t *testing.T) {
	if Bin("WIDGET") != "A-01-03" {
		t.Fatal("bin lookup wrong")
	}
}
''')

# --- the defect-adjacent packages -----------------------------------------

w("internal/pricing/pricing.go", '''// Package pricing owns the money arithmetic for a single line:
// quantity times the catalog's per-unit cents. Keeping it out of
// orders keeps the totaling rule in one place — and makes it the
// package a cross-package hunt has to reach.
package pricing

// LineTotal returns the line's total in cents: qty × unit cents.
func LineTotal(qty, unitCents int) int {
	return qty * unitCents
}

// BulkRate is the markdown percent a line earns at quantity —
// unrelated to LineTotal's contract, tested below so the package
// still carries its own green signal.
func BulkRate(qty int) int {
	switch {
	case qty >= 50:
		return 15
	case qty >= 10:
		return 5
	default:
		return 0
	}
}
''')

w("internal/pricing/pricing_test.go", '''package pricing

import "testing"

// BulkRate is the only rule this test pins — LineTotal is
// exercised downstream, in the orders suite, which is where a
// regression here surfaces first.
func TestBulkRate(t *testing.T) {
	if got := BulkRate(10); got != 5 {
		t.Fatalf("BulkRate = %d", got)
	}
	if got := BulkRate(60); got != 15 {
		t.Fatalf("BulkRate = %d", got)
	}
}
''')

w("internal/orders/orders.go", '''// Package orders assembles lines into a total. The per-line
// arithmetic is delegated to pricing — an order doesn't know how
// a line's cents are computed, only that they are.
package orders

import (
	"shop/internal/catalog"
	"shop/internal/pricing"
)

// Line is one order row.
type Line struct {
	Item catalog.Item
	Qty  int
}

// Order is the assembled purchase.
type Order struct {
	Lines []Line
}

// Total sums each line's priced total.
func Total(o Order) int {
	sum := 0
	for _, l := range o.Lines {
		sum += pricing.LineTotal(l.Qty, l.Item.PriceCents)
	}
	return sum
}
''')

w("internal/orders/orders_test.go", '''package orders

import (
	"testing"

	"shop/internal/catalog"
)

func TestOrderTotal(t *testing.T) {
	widget, _ := catalog.Lookup("WIDGET") // 595c
	gadget, _ := catalog.Lookup("GADGET") // 250c
	o := Order{Lines: []Line{
		{Item: widget, Qty: 2}, // 1190
		{Item: gadget, Qty: 1}, //  250
	}}
	if got := Total(o); got != 1440 {
		t.Fatalf("Total = %d, want 1440", got)
	}
}
''')

w("internal/reports/reports.go", '''// Package reports renders the plain-text sales summaries ops
// reads. Generate's signature is deliberately single-argument —
// the call sites in audit rely on it.
package reports

import (
	"fmt"
	"time"
)

// ReportOpts scopes the report.
type ReportOpts struct {
	Days       int
	IncludeTax bool
}

// Generate renders the report body.
func Generate(opts ReportOpts) (string, error) {
	if opts.Days <= 0 {
		return "", fmt.Errorf("days must be positive")
	}
	tax := "excluded"
	if opts.IncludeTax {
		tax = "included"
	}
	return fmt.Sprintf("sales report: %d day(s), tax %s (as of %s)",
		opts.Days, tax, time.Now().UTC().Format("2006-01-02")), nil
}
''')

w("internal/reports/report_test.go", '''package reports

import (
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	body, err := Generate(ReportOpts{Days: 7, IncludeTax: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "7 day") {
		t.Fatalf("body = %q", body)
	}
	if _, err := Generate(ReportOpts{Days: 0}); err == nil {
		t.Fatal("zero-day report allowed")
	}
}
''')

w("internal/audit/audit.go", '''// Package audit emits the compliance trail — including the sales
// report, which it requests through reports.Generate's one-arg
// signature. That call is what pins the signature in place.
package audit

import (
	"shop/internal/reports"
)

// Daily produces today's audit summary.
func Daily() (string, error) {
	return reports.Generate(reports.ReportOpts{Days: 1, IncludeTax: true})
}
''')

w("internal/audit/audit_test.go", '''package audit

import (
	"strings"
	"testing"
)

func TestDaily(t *testing.T) {
	body, err := Daily()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "1 day") {
		t.Fatalf("body = %q", body)
	}
}
''')

# --- the defects ----------------------------------------------------------

w(".defects/ft/internal/pricing/pricing.go", '''// Package pricing owns the money arithmetic for a single line:
// quantity times the catalog's per-unit cents. Keeping it out of
// orders keeps the totaling rule in one place — and makes it the
// package a cross-package hunt has to reach.
package pricing

// LineTotal returns the line's total in cents: qty × unit cents.
func LineTotal(qty, unitCents int) int {
	return qty * unitCents / 100
}

// BulkRate is the markdown percent a line earns at quantity —
// unrelated to LineTotal's contract, tested below so the package
// still carries its own green signal.
func BulkRate(qty int) int {
	switch {
	case qty >= 50:
		return 15
	case qty >= 10:
		return 5
	default:
		return 0
	}
}
''')

w(".defects/bb/internal/reports/report_test.go", '''package reports

import (
	"context"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	body, err := Generate(context.Background(), ReportOpts{Days: 7, IncludeTax: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "7 day") {
		t.Fatalf("body = %q", body)
	}
	if _, err := Generate(context.Background(), ReportOpts{Days: 0}); err == nil {
		t.Fatal("zero-day report allowed")
	}
}
''')

print(f"fixture written under {ROOT}")
