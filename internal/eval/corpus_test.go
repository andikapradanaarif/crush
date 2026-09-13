package eval

import (
	"context"
	"path/filepath"
	"testing"
)

func TestExampleCorpus_QuarantineClean(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "eval", "corpus", "fix-nil-map-write"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := LoadTrajectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{EvalDir: "../..", QuarantineRepeats: 2, WorkParent: t.TempDir()}
	reason, err := r.Quarantine(context.Background(), tr, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quarantine verdict: %q", reason)
}
