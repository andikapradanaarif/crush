package stamp

import (
	"testing"

	"example.com/papertrail/internal/doc"
)

func TestStampAll(t *testing.T) {
	d := &doc.Doc{Title: "x", Pages: []string{"a", "b", "c"}}
	got := StampAll(d)
	if len(got) != 3 {
		t.Fatalf("StampAll len = %d; want 3", len(got))
	}
	want := []string{"page 1 of 3", "page 2 of 3", "page 3 of 3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("StampAll[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}
