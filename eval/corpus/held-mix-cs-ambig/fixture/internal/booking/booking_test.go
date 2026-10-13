package booking

import (
	"testing"

	"example.com/studio/internal/slot"
)

func TestConfirmRejectsOverlap(t *testing.T) {
	taken, ok := Confirm(nil, "ani", slot.Slot{Start: 9, End: 11})
	if !ok {
		t.Fatal("first booking must confirm")
	}
	if _, ok := Confirm(taken, "budi", slot.Slot{Start: 10, End: 12}); ok {
		t.Fatal("overlapping booking must be rejected")
	}
	if len(taken) != 1 {
		t.Fatalf("rejected booking still mutated list: %d entries", len(taken))
	}
}

func TestConfirmAdjacent(t *testing.T) {
	taken, _ := Confirm(nil, "ani", slot.Slot{Start: 9, End: 10})
	if _, ok := Confirm(taken, "budi", slot.Slot{Start: 10, End: 11}); !ok {
		t.Fatal("adjacent booking must confirm")
	}
}
