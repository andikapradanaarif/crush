package eval

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepoExperiments_LoadWithPrimary(t *testing.T) {
	for _, name := range []string{"notebook-checkpoint", "notebook-prior-turns-digest"} {
		e, err := LoadExperiment(filepath.Join("..", "..", "eval", "experiments", name+".json"))
		require.NoError(t, err, name)
		require.NotNil(t, e.Primary, name)
		require.Equal(t, "request.prompt_tokens_peak", e.Primary.Metric)
	}
}
