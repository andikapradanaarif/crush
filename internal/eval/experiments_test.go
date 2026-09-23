package eval

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExperiments_ManifestConsistent validates every pinned experiment
// against the flags manifest — arm options must name declared flags so
// baseline keys rotate correctly, and arm coverage must resolve
// against the grammar the run record actually carries.
func TestExperiments_ManifestConsistent(t *testing.T) {
	t.Parallel()
	manifest, err := LoadFlagsManifest("../../eval")
	require.NoError(t, err)
	corpus, err := LoadCorpus("../../eval")
	require.NoError(t, err)
	bands, err := LoadBands("../../eval")
	require.NoError(t, err)
	paths, err := filepath.Glob("../../eval/experiments/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, p := range paths {
		exp, err := LoadExperiment(p)
		require.NoError(t, err, p)
		require.NoError(t, manifest.ValidateArmFlags(exp), p)
		require.NoError(t, ValidateArmCoverageResolved(exp, manifest), p)
		// Arm coverage must be achievable on the corpus the selectors
		// resolve — a predicate past the turn-count ceiling starves
		// every run of that trajectory.
		trajs, err := SelectCorpus(corpus, bands, exp.Corpus)
		require.NoError(t, err, p)
		require.NoError(t, ValidateArmCoverageVsCorpus(exp, trajs), p)
	}
}
