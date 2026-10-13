package main

import (
	"fmt"

	"example.com/studio/internal/slot"
)

func main() {
	s := slot.Slot{Start: 9, End: 11}
	fmt.Println("slot:", s.Start, "-", s.End)
}
