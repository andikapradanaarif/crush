package eval

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCorpus_QuarantineClean(t *testing.T) {
	evalDir, err := filepath.Abs(filepath.Join("..", "..", "eval"))
	require.NoError(t, err)
	corpus, err := LoadCorpus(evalDir)
	require.NoError(t, err)
	require.NotEmpty(t, corpus)

	for id, tr := range corpus {
		tr := tr
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			for _, tool := range tr.Requires.Tools {
				if _, err := exec.LookPath(tool); err != nil {
					t.Skipf("required tool %q not on PATH", tool)
				}
			}
			if len(tr.Requires.OS) > 0 && !slices.Contains(tr.Requires.OS, runtime.GOOS) {
				t.Skipf("requires os %v, running on %s", tr.Requires.OS, runtime.GOOS)
			}
			r := &Runner{EvalDir: evalDir, QuarantineRepeats: 2, WorkParent: t.TempDir()}
			t.Cleanup(r.Close)
			dir := filepath.Join(evalDir, "corpus", id)
			reason, err := r.Quarantine(context.Background(), tr, dir)
			require.NoError(t, err)
			// A rotted seed must fail CI, not just log.
			require.Empty(t, reason, "trajectory %s quarantined: %s", id, reason)
		})
	}
}
