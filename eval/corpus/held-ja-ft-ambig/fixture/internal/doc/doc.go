// Package doc models a stamped document.
package doc

// Doc is a titled list of page bodies.
type Doc struct {
	Title  string
	Pages  []string
}

// PageCount reports how many pages the document holds.
func (d *Doc) PageCount() int { return len(d.Pages) }

// Page returns the 1-indexed page body; out-of-range reads are "".
func (d *Doc) Page(i int) string {
	if i < 1 || i > len(d.Pages) {
		return ""
	}
	return d.Pages[i-1]
}
