package warehouse

import "testing"

func TestBin(t *testing.T) {
	if Bin("WIDGET") != "A-01-03" {
		t.Fatal("bin lookup wrong")
	}
}
