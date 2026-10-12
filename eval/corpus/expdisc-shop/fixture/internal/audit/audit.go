// Package audit emits the compliance trail — including the sales
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
