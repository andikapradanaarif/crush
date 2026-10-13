package doc

import "testing"

func TestPageIsOneIndexed(t *testing.T) {
	d := &Doc{Title: "x", Pages: []string{"a", "b", "c"}}
	if d.PageCount() != 3 {
		t.Fatalf("PageCount = %d; want 3", d.PageCount())
	}
	if got := d.Page(1); got != "a" {
		t.Fatalf("Page(1) = %q; want a", got)
	}
	if got := d.Page(0); got != "" {
		t.Fatalf("Page(0) = %q; want empty", got)
	}
}
