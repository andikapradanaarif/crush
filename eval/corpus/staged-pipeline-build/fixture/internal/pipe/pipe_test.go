package pipe

import (
	"reflect"
	"testing"
)

func TestNormalizeTrimsAndLowercases(t *testing.T) {
	got := normalize([]string{"  Go  ", "LANG ", "Lang"})
	want := []string{"go", "lang", "lang"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize = %v, want %v", got, want)
	}
}

func TestTokenizeSplitsOnWhitespace(t *testing.T) {
	got := tokenize([]string{"a b", "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
}

func TestRunAppliesStages(t *testing.T) {
	p := New(Config{})
	got := p.Run([]string{"a b"})
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Run = %v, want %v", got, want)
	}
}
