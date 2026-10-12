package currency

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
