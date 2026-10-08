package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/params"
	"github.com/charmbracelet/crush/internal/shell"
)

// The generator must emit instances indistinguishable from
// hand-written cells: valid at load, materializable, and their
// authored seed state must satisfy their own gate — otherwise the
// ladder's rows silently answer a different question.
func TestGenerate_EndToEndSeedsAndGate(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain in the generated fixture")
	}
	for _, spec := range []GenSpec{
		// Full-dose cell: every relevant row plus high-plausibility
		// distractors — the richest authored state.
		{Quirks: 4, Distractors: 2, Plausibility: "high", Prompt: "vague", Depth: 2, Seed: 7, Replicate: 0},
		// Low-tier distractors are benign command rows — no
		// failures among them, so open stays at the target count.
		{Quirks: 1, Distractors: 3, Plausibility: "low", Prompt: "explicit", Depth: 0, Seed: 11, Replicate: 0},
		// In-scope distractors are hidden decoy tests inside the
		// target package — open rows that bind under explicit
		// scope, the only wrong-memory class that renders.
		{Quirks: 2, Distractors: 2, Plausibility: "in_scope", Prompt: "explicit", Depth: 0, Seed: 13, Replicate: 0},
		// Bare cell: no memory at all — the M=0 arm's honest floor.
		{Quirks: 0, Distractors: 0, Plausibility: "mid", Prompt: "vague", Depth: 1, Seed: 3, Replicate: 0},
	} {
		corpus := t.TempDir()
		dir, err := Generate(corpus, spec)
		require.NoError(t, err)
		traj, err := LoadTrajectory(dir)
		require.NoError(t, err, "generated instance must validate: %s", spec.genID())

		r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir()}
		workdir, err := Materialize(context.Background(), traj, dir, t.TempDir(), r.checkEnv())
		require.NoError(t, err)
		defer func() {
			os.RemoveAll(workdir)
			os.RemoveAll(DataDirFor(workdir))
		}()

		ids, err := r.runScriptedSeeds(context.Background(), workdir, traj.SeedCommands, "eval-test")
		require.NoError(t, err)
		require.Len(t, ids, len(traj.SeedCommands))

		// Unseeded cells carry no gate — nothing authored to assert.
		if traj.Check.SeedScript != "" {
			schk := runCheckScript(context.Background(), traj.Check.SeedScript, dir, workdir, r.checkEnv(), checkTimeout(traj))
			require.NoError(t, schk.Err)
			require.Equal(t, 0, schk.Exit,
				"generated gate rejected its own authored state (%s): %s", spec.genID(), schk.Stderr)
		}

		// The check contract: post-seed state fails only on the
		// target (the sibling's seed fix already landed), and the
		// real fix flips it green — otherwise the cell measures
		// nothing or scores a non-fix.
		chk := runCheckScript(context.Background(), traj.Check.Script, dir, workdir, r.checkEnv(), checkTimeout(traj))
		require.NoError(t, chk.Err)
		require.NotEqual(t, 0, chk.Exit,
			"check passed pre-fix on %s — the cell has nothing to measure", spec.genID())
		d := spec.draw()
		fix := fmt.Sprintf("printf 'package %s\\n\\n// Contract is the settlement constant.\\nfunc Contract() int {\\n\\treturn 42\\n}\\n' > %s/contract.go",
			d.target, d.targetPath)
		res, err := shell.RunAndCapture(context.Background(), shell.RunOptions{
			Command: fix, Cwd: workdir, Env: r.checkEnv(),
		})
		require.NoError(t, err)
		require.True(t, res.Verdict)
		require.Equal(t, 0, res.ExitCode)
		chk = runCheckScript(context.Background(), traj.Check.Script, dir, workdir, r.checkEnv(), checkTimeout(traj))
		require.NoError(t, chk.Err)
		require.Equal(t, 0, chk.Exit,
			"check rejected the real fix on %s: %s", spec.genID(), chk.Stderr)
	}
}

// Replicate draws must differ (fresh quirk identity per instance)
// while the same (seed, replicate) reproduces byte-identical dirs —
// the property that makes snapshot keys stable per instance and
// variance live across instances.
func TestGenerate_ReplicateVarianceAndDeterminism(t *testing.T) {
	t.Parallel()
	spec := GenSpec{Quirks: 2, Distractors: 2, Plausibility: "high", Prompt: "vague", Depth: 1, Seed: 42}

	c1 := t.TempDir()
	d1, err := Generate(c1, spec)
	require.NoError(t, err)
	spec.Replicate = 1
	d2, err := Generate(c1, spec)
	require.NoError(t, err)
	spec.Replicate = 0

	// Different draws land different target names somewhere in the
	// instance — comparing the trajectory text catches the sampled
	// referents.
	t1, _ := os.ReadFile(filepath.Join(d1, "trajectory.json"))
	t2, _ := os.ReadFile(filepath.Join(d2, "trajectory.json"))
	require.NotEqual(t, string(t1), string(t2),
		"replicates must sample different quirk identities")

	// Same spec → same dir contents (modulo nothing — fully
	// deterministic draw).
	c2 := t.TempDir()
	e1, err := Generate(c2, spec)
	require.NoError(t, err)
	t3, _ := os.ReadFile(filepath.Join(e1, "trajectory.json"))
	require.Equal(t, string(t1), string(t3),
		"same seed+replicate must reproduce byte-identical specs")
}

// Dimension validation: a cell that can't express its dose must
// fail loudly rather than silently clamp to a different question.
func TestGenSpec_Validate(t *testing.T) {
	t.Parallel()
	ok := GenSpec{Quirks: 4, Distractors: 50, Plausibility: "high", Prompt: "explicit", Depth: 4}
	require.NoError(t, ok.Validate())
	for _, bad := range []GenSpec{
		{Quirks: 5, Distractors: 0, Plausibility: "low", Prompt: "vague"},
		{Quirks: -1, Distractors: 0, Plausibility: "low", Prompt: "vague"},
		{Quirks: 0, Distractors: len(genDistractorNames) + 1, Plausibility: "low", Prompt: "vague"},
		{Quirks: 0, Distractors: 0, Plausibility: "wild", Prompt: "vague"},
		{Quirks: 0, Distractors: 0, Plausibility: "low", Prompt: "chatty"},
		{Quirks: 0, Distractors: 0, Plausibility: "low", Prompt: "vague", Depth: 5},
	} {
		require.Error(t, bad.Validate(), "%+v should reject", bad)
	}
}

func TestGenSpec_SeedAgesInsideTTL(t *testing.T) {
	t.Parallel()
	// A seed backdated past the read-side open-failure TTL is dead
	// state — the fetch drops it before the selector sees it, so
	// the stored dose silently collapses. Pin the whole authored
	// arc inside the default bound at the ladder's top dose.
	spec := GenSpec{
		Quirks: 4, Distractors: len(genDistractorNames),
		Plausibility: "high", Prompt: "explicit", Seed: 1,
	}
	seeds := spec.seedCommands(spec.draw())
	require.NotEmpty(t, seeds)
	ttl := params.DefaultMemory().OpenFailureTTL
	for _, s := range seeds {
		require.Less(t, s.AgoSeconds, ttl.Seconds(),
			"seed %q exceeds open_failure_ttl", s.Commands)
	}
}
