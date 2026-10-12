package sessions

import "testing"

func TestRoundTrip(t *testing.T) {
	Open("amy", "t1")
	if u, ok := Who("t1"); !ok || u != "amy" {
		t.Fatal("token didn't resolve")
	}
}
