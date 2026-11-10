package eval

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

var updateTB21 = flag.Bool("update-tb21", false, "regenerate the frozen TB2.1 split manifests")

// TestTB21SplitFrozen pins the dev/sealed draw to the committed manifests:
// re-deriving from the vendored source + recorded seed must reproduce both
// files byte-for-byte. Regenerate deliberately with -update-tb21.
func TestTB21SplitFrozen(t *testing.T) {
	evalDir := "../../eval"
	src, err := LoadTB21TaskSource(evalDir)
	require.NoError(t, err)
	require.Len(t, src.Tasks, 89, "vendored TB2.1 corpus should hold 89 tasks")
	require.True(t, slices.IsSorted(src.Tasks), "vendored task list should be sorted")

	dev := TB21DevManifest(src, TB21Seed, TB21DevCount)
	sealed := TB21SealedManifest(src, TB21Seed, TB21DevCount)

	if *updateTB21 {
		dir := filepath.Join(evalDir, TB21ManifestsDir)
		require.NoError(t, WriteTB21Manifest(dev, filepath.Join(dir, "tb21-dev.json")))
		require.NoError(t, WriteTB21Manifest(sealed, filepath.Join(dir, "tb21-sealed.json")))
		return
	}

	for _, want := range []*TB21Manifest{dev, sealed} {
		path := filepath.Join(evalDir, TB21ManifestsDir, "tb21-"+want.Split+".json")
		data, err := os.ReadFile(path)
		require.NoError(t, err, "frozen manifest missing — regenerate with -update-tb21")
		var got TB21Manifest
		require.NoError(t, json.Unmarshal(data, &got))
		require.Equal(t, want, &got, "%s drifted from the seeded draw — freeze is broken", path)
		require.Equal(t, TB21TaskHash(got.Tasks), got.TaskSHA256, "%s embedded hash stale", path)
	}

	require.Len(t, dev.Tasks, TB21DevCount)
	require.Len(t, sealed.Tasks, len(src.Tasks)-TB21DevCount)
	seen := map[string]string{}
	for _, task := range dev.Tasks {
		seen[task] = "dev"
	}
	for _, task := range sealed.Tasks {
		_, dup := seen[task]
		require.False(t, dup, "task %s dealt into both splits", task)
		seen[task] = "sealed"
	}
	require.Equal(t, TB21AccessSealed, sealed.AccessProtocol)
}

// TestTB21SplitDeterminism guards the draw itself: same inputs must deal
// the same partition, and distinct seeds must not accidentally coincide.
func TestTB21SplitDeterminism(t *testing.T) {
	src, err := LoadTB21TaskSource("../../eval")
	require.NoError(t, err)

	devA, sealedA := TB21SplitTasks(src.Tasks, TB21Seed, TB21DevCount)
	devB, sealedB := TB21SplitTasks(src.Tasks, TB21Seed, TB21DevCount)
	require.Equal(t, devA, devB)
	require.Equal(t, sealedA, sealedB)

	devOther, _ := TB21SplitTasks(src.Tasks, TB21Seed+1, TB21DevCount)
	require.NotEqual(t, devA, devOther, "adjacent seed dealt the identical dev split")
}

// TestTB21LoadManifest checks the consumer path: round-trip load verifies
// the embedded digest, and a tampered task list is rejected.
func TestTB21LoadManifest(t *testing.T) {
	dev, err := LoadTB21Manifest("../../eval", "dev")
	require.NoError(t, err)
	require.Len(t, dev.Tasks, TB21DevCount)
	require.Equal(t, TB21AccessDev, dev.AccessProtocol)

	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, TB21ManifestsDir), 0o755))
	dev.Tasks = append(dev.Tasks, "not-in-corpus")
	require.NoError(t, WriteTB21Manifest(dev, filepath.Join(tmp, TB21ManifestsDir, "tb21-dev.json")))
	_, err = LoadTB21Manifest(tmp, "dev")
	require.ErrorContains(t, err, "task hash mismatch")
}
