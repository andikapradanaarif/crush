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

	// The sealed manifest carries no list — the holdout is derived
	// here for the invariant checks only.
	sealedTasks := func() []string {
		_, s := TB21SplitTasks(src.Tasks, TB21Seed, TB21DevCount)
		return s
	}()

	// Invariants run before any regeneration — a bad draw must
	// never get frozen into the manifests.
	require.Len(t, dev.Tasks, TB21DevCount)
	require.Len(t, sealedTasks, len(src.Tasks)-TB21DevCount)
	require.Empty(t, sealed.Tasks, "sealed manifest must not carry the task list")
	require.Equal(t, TB21TaskHash(sealedTasks), sealed.TaskSHA256)
	seen := map[string]string{}
	for _, task := range dev.Tasks {
		seen[task] = "dev"
	}
	for _, task := range sealedTasks {
		_, dup := seen[task]
		require.False(t, dup, "task %s dealt into both splits", task)
		seen[task] = "sealed"
	}
	require.Equal(t, TB21AccessSealed, sealed.AccessProtocol)

	if *updateTB21 {
		dir := filepath.Join(evalDir, TB21ManifestsDir)
		require.NoError(t, WriteTB21Manifest(dev, filepath.Join(dir, "tb21-dev.json")))
		require.NoError(t, WriteTB21Manifest(sealed, filepath.Join(dir, "tb21-sealed.json")))
		return
	}

	// Compare against the committed files, not the loader's populated
	// view — drift must be caught in the artifact itself.
	for _, want := range []*TB21Manifest{dev, sealed} {
		path := filepath.Join(evalDir, TB21ManifestsDir, "tb21-"+want.Split+".json")
		data, err := os.ReadFile(path)
		require.NoError(t, err, "frozen manifest missing — regenerate with -update-tb21")
		var got TB21Manifest
		require.NoError(t, json.Unmarshal(data, &got))
		require.Equal(t, want, &got, "%s drifted from the seeded draw — freeze is broken", path)
		_, err = LoadTB21Manifest(evalDir, want.Split)
		require.NoError(t, err, "committed %s manifest fails its own verification", want.Split)
	}

	// The sealed file must not enumerate the holdout — the list is
	// derived at load, never committed to the repo.
	sealedData, err := os.ReadFile(filepath.Join(evalDir, TB21ManifestsDir, "tb21-sealed.json"))
	require.NoError(t, err)
	require.NotContains(t, string(sealedData), `"tasks"`)
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

// TestTB21LoadManifest checks the consumer path: round-trip loads verify,
// and every tamper shape fails — list drift, stale digest, wrong seed, and
// a sealed file that enumerates the holdout.
func TestTB21LoadManifest(t *testing.T) {
	evalDir := "../../eval"
	dev, err := LoadTB21Manifest(evalDir, "dev")
	require.NoError(t, err)
	require.Len(t, dev.Tasks, TB21DevCount)
	require.Equal(t, TB21AccessDev, dev.AccessProtocol)

	sealed, err := LoadTB21Manifest(evalDir, "sealed")
	require.NoError(t, err)
	require.Len(t, sealed.Tasks, 30, "sealed load re-derives the holdout")

	src, err := LoadTB21TaskSource(evalDir)
	require.NoError(t, err)

	writeAt := func(t *testing.T, m *TB21Manifest, name string) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, TB21ManifestsDir), 0o755))
		srcData, err := json.Marshal(src)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, TB21ManifestsDir, "tb21-tasks.json"), srcData, 0o644))
		require.NoError(t, WriteTB21Manifest(m, filepath.Join(dir, TB21ManifestsDir, name)))
		return dir
	}

	t.Run("extra task differs from draw", func(t *testing.T) {
		m := *dev
		m.Tasks = append(slices.Clone(dev.Tasks), "not-in-corpus")
		dir := writeAt(t, &m, "tb21-dev.json")
		_, err := LoadTB21Manifest(dir, "dev")
		require.ErrorContains(t, err, "differs from the seeded draw")
	})

	t.Run("stale digest", func(t *testing.T) {
		m := *dev
		m.TaskSHA256 = "00"
		dir := writeAt(t, &m, "tb21-dev.json")
		_, err := LoadTB21Manifest(dir, "dev")
		require.ErrorContains(t, err, "task hash mismatch")
	})

	t.Run("wrong seed", func(t *testing.T) {
		m := *dev
		m.Seed = TB21Seed + 1
		dir := writeAt(t, &m, "tb21-dev.json")
		_, err := LoadTB21Manifest(dir, "dev")
		require.ErrorContains(t, err, "frozen values")
	})

	t.Run("sealed enumerates tasks", func(t *testing.T) {
		m := *sealed
		m.Tasks = sealed.Tasks
		// WriteTB21Manifest itself refuses this — simulate a foreign file.
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, TB21ManifestsDir), 0o755))
		srcData, err := json.Marshal(src)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, TB21ManifestsDir, "tb21-tasks.json"), srcData, 0o644))
		mData, err := json.Marshal(&m)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, TB21ManifestsDir, "tb21-sealed.json"), mData, 0o644))
		_, err = LoadTB21Manifest(dir, "sealed")
		require.ErrorContains(t, err, "must not enumerate")

		require.ErrorContains(t, WriteTB21Manifest(&m, filepath.Join(dir, "x.json")), "un-enumerated")
	})
}
