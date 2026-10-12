package reports

import (
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	body, err := Generate(ReportOpts{Days: 7, IncludeTax: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "7 day") {
		t.Fatalf("body = %q", body)
	}
	if _, err := Generate(ReportOpts{Days: 0}); err == nil {
		t.Fatal("zero-day report allowed")
	}
}
