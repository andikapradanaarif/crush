// Package stamp renders page footers for documents.
package stamp

import (
	"fmt"

	"example.com/papertrail/internal/doc"
)

// Footer renders one page's footer — "page i of n", 1-indexed.
func Footer(page, total int) string {
	return fmt.Sprintf("page %d of %d", page, total)
}

// StampAll returns a footer for every page in the document.
func StampAll(d *doc.Doc) []string {
	out := make([]string, 0, d.PageCount())
	for i := 0; i < d.PageCount(); i++ {
		out = append(out, Footer(i, d.PageCount()))
	}
	return out
}
