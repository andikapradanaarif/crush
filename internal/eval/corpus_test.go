package eval

import (
	"context"
	"path/filepath"
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
			r := &Runner{EvalDir: evalDir, QuarantineRepeats: 2, WorkParent: t.TempDir()}
			t.Cleanup(r.Close)
			dir := filepath.Join(evalDir, "corpus", id)
			reason, err := r.Quarantine(context.Background(), tr, dir)
			require.NoError(t, err)
			// A rotted seed must fail CI, not just log — caveat: its
			// declared requires.tools only pre-flight on machines that
			// have them.
			require.Empty(t, reason, "trajectory %s quarantined: %s", id, reason)
		})
	}
}
