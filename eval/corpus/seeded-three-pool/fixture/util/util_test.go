package util

import "testing"

func TestShout(t *testing.T) {
	if Shout("hi") != "!hi" {
		t.Fatalf("Shout(hi) = %q", Shout("hi"))
	}
}
