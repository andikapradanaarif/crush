package slot

import "testing"

func TestOverlaps(t *testing.T) {
	if !Overlaps(Slot{9, 11}, Slot{10, 12}) {
		t.Fatal("9-11 must overlap 10-12")
	}
	if Overlaps(Slot{9, 10}, Slot{10, 11}) {
		t.Fatal("adjacent slots must not overlap")
	}
}
