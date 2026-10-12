package audit

import (
	"strings"
	"testing"
)

func TestDaily(t *testing.T) {
	body, err := Daily()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "1 day") {
		t.Fatalf("body = %q", body)
	}
}
