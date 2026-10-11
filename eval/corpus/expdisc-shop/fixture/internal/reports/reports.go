// Package reports renders the plain-text sales summaries ops
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
