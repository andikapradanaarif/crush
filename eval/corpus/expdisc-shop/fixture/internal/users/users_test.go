package users

import "testing"

func TestByID(t *testing.T) {
	if u, ok := ByID("u1"); !ok || u.Zone != "standard" {
		t.Fatal("u1 lookup wrong")
	}
}
