// Offline selector simulation (#223): the memory ladders split into
// a free selector half and a paid agent half — what a cell's stored
// dose renders under a given prompt and parameter set answers without
// a model call. RunSelectProbe materializes + script-seeds the
// trajectory once into a persistent genwork dir, then runs the real
// production selector (agent.SimulateSelection) over the seeded
// crush.db — the same binding rules, render budgets, and decision
// records a live turn produces.
package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
)

// EnsureSeeded materializes the trajectory under
// <evaldir>/genwork/<id>/ and runs its scripted seeds — once; a
// later call reuses the seeded dir. This is the offline tier's
// equivalent of the paid path's snapshot: seeds are free, so the
// seeded state persists rather than re-materializing per probe.
// Returns the workdir and the trajectory's pinned project key.
func (r *Runner) EnsureSeeded(ctx context.Context, traj *Trajectory, trajDir string) (workdir, seedKey string, err error) {
	contentHash, err := ContentHash(trajDir, trajContentRefs(trajDir, traj)...)
	if err != nil {
		return "", "", fmt.Errorf("content hash: %w", err)
	}
	seedKey = "eval-" + contentHash

	parent := filepath.Join(r.EvalDir, "genwork", traj.ID)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", err
	}
	// Reuse a seeded workdir — the seeded.ok marker means the seeds
	// finished, not merely started: a db mid-write would otherwise
	// pass for seeded state and the probe would read torn rows.
	// Anything unmarked is a torn attempt — wipe and re-materialize
	// rather than trust a partial seed.
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			cand := filepath.Join(parent, e.Name())
			if !e.IsDir() {
				continue
			}
			if marker, err := os.ReadFile(filepath.Join(cand, seedDoneMarker)); err == nil &&
				strings.TrimSpace(string(marker)) == seedKey {
				return cand, seedKey, nil
			}
		}
	}
	workdir, err = Materialize(ctx, traj, trajDir, parent, r.checkEnv())
	if err != nil {
		return "", "", fmt.Errorf("materialize: %w", err)
	}
	if len(traj.SeedCommands) > 0 {
		if _, err := r.runScriptedSeeds(ctx, workdir, traj.SeedCommands, seedKey); err != nil {
			os.RemoveAll(workdir)
			os.RemoveAll(DataDirFor(workdir))
			return "", "", fmt.Errorf("seed_commands: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, seedDoneMarker), []byte(seedKey+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return workdir, seedKey, nil
}

// seedDoneMarker names the file EnsureSeeded writes after the last
// seed command lands — the reuse check keys on it, never on the
// db's mere existence.
const seedDoneMarker = ".seeded.ok"

// SelectProbeReport is one offline selector measurement: every
// evaluated candidate with its pool and verdict, plus the pool
// tallies the ladder curve reads.
type SelectProbeReport struct {
	Trajectory string            `json:"trajectory"`
	Prompt     string            `json:"prompt"`
	Params     params.Memory     `json:"params"`
	Workdir    string            `json:"workdir"`
	Candidates []SelectCandidate `json:"candidates"`
	Counts     map[string]int    `json:"counts"`
}

// SelectCandidate is one memory row's simulated verdict.
type SelectCandidate struct {
	Pool    string `json:"pool"`
	Cmd     string `json:"cmd"`
	Admit   bool   `json:"admit"`
	Reason  string `json:"reason"`
	Settled string `json:"settled_by,omitempty"`
}

// RunSelectProbe executes the offline selector measurement: ensure
// the trajectory's seeded state exists, fetch the memory pools under
// the resolved parameter set — honoring the zero-render-limit
// pool suppression the production fetch path applies — then run the
// real selector over them.
func (r *Runner) RunSelectProbe(ctx context.Context, traj *Trajectory, trajDir, prompt string, overlay map[string]any) (*SelectProbeReport, error) {
	mp, err := params.ResolveMemory(overlay)
	if err != nil {
		return nil, fmt.Errorf("memory params: %w", err)
	}
	workdir, seedKey, err := r.EnsureSeeded(ctx, traj, trajDir)
	if err != nil {
		return nil, err
	}
	if prompt == "" && len(traj.Task.Turns) > 0 {
		prompt = traj.Task.Turns[0]
	}

	dataDir := DataDirFor(workdir)
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return nil, fmt.Errorf("open seeded db: %w", err)
	}
	defer func() { _ = db.Release(dataDir) }()

	svc := cmdlog.NewService(db.New(conn), workdir, mp,
		cmdlog.WithProjectKey(seedKey))
	var pools agent.MemoryPools
	if mp.OpenRenderLimit > 0 {
		pools.Open, err = svc.ListOpenFailures(ctx, mp.FetchLimit)
		if err != nil {
			return nil, fmt.Errorf("open pool: %w", err)
		}
	}
	if mp.ResolvedRenderLimit > 0 {
		pools.Resolved, _ = svc.ListResolvedFailures(ctx, mp.FetchLimit)
	}
	if mp.CommandRenderLimit > 0 {
		pools.Commands, _ = svc.ListCommands(ctx, mp.FetchLimit)
	}

	admitted, decisions := agent.SimulateSelection(prompt, pools, workdir,
		agent.MemoryRenderLimits{
			Open:     mp.OpenRenderLimit,
			Resolved: mp.ResolvedRenderLimit,
			Command:  mp.CommandRenderLimit,
		})

	rep := &SelectProbeReport{
		Trajectory: traj.ID,
		Prompt:     prompt,
		Params:     mp,
		Workdir:    workdir,
		Counts:     map[string]int{},
	}
	seen := map[string]int{"open_admit": len(admitted.Open), "resolved_admit": len(admitted.Resolved), "command_admit": len(admitted.Commands)}
	for pool, n := range seen {
		rep.Counts[pool] = n
	}
	for _, d := range decisions {
		rep.Candidates = append(rep.Candidates, SelectCandidate{
			Pool: d.Pool, Cmd: d.Cmd, Admit: d.Admit, Reason: d.Reason, Settled: d.SettledBy,
		})
		key := d.Pool + "_seen"
		rep.Counts[key]++
		if d.Reason == "render_capped" {
			rep.Counts[d.Pool+"_capped"]++
		}
	}
	rep.Counts["candidates"] = len(decisions)
	return rep, nil
}

// String renders the probe as a compact table — pool counts plus a
// per-candidate verdict line.
func (r *SelectProbeReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "== select %s\nprompt: %s\n", r.Trajectory, r.Prompt)
	for _, k := range sortedKeys(r.Counts) {
		fmt.Fprintf(&b, "%s=%d  ", k, r.Counts[k])
	}
	if len(r.Counts) > 0 {
		b.WriteByte('\n')
	}
	for _, c := range r.Candidates {
		admit := "reject"
		if c.Admit {
			admit = "ADMIT"
		}
		fmt.Fprintf(&b, "%-8s %-50s %-6s %s\n", c.Pool, c.Cmd, admit, c.Reason)
	}
	return b.String()
}

// SelectSweepSpec is the dose grid the offline curves run over —
// each combination × replicate becomes one generated instance plus
// one selector probe. Kept as GenSpec fields so the grid is the
// generator's parameter surface verbatim.
type SelectSweepSpec struct {
	Quirks       []int    `json:"quirks"`
	Distractors  []int    `json:"distractors"`
	Plausibility []string `json:"plausibility"`
	Prompt       []string `json:"prompt"`
	Depth        []int    `json:"depth"`
	Replicates   int      `json:"replicates"`
	Seed         int64    `json:"seed"`
}

// SelectSweepRow is one cell's measurement — the spec that produced
// the instance plus its probe report, so the curve aggregation reads
// dose → verdict straight off the row.
type SelectSweepRow struct {
	Cell   GenSpec            `json:"cell"`
	Report *SelectProbeReport `json:"report"`
	Error  string             `json:"error,omitempty"`
}

// RunSelectSweep enumerates the dose grid, generates each cell's
// instances into corpusRoot, and probes each — the free half of the
// memory ladders as one pass. Cells stream back in order; a failing
// cell records its error and the sweep continues — one bad instance
// must not void the rest of the curve.
func (r *Runner) RunSelectSweep(ctx context.Context, corpusRoot string, spec SelectSweepSpec, overlay map[string]any, onRow func(SelectSweepRow)) error {
	if len(spec.Quirks) == 0 {
		spec.Quirks = []int{4}
	}
	if len(spec.Distractors) == 0 {
		spec.Distractors = []int{0}
	}
	if len(spec.Plausibility) == 0 {
		spec.Plausibility = []string{"mid"}
	}
	if len(spec.Prompt) == 0 {
		spec.Prompt = []string{"vague"}
	}
	if len(spec.Depth) == 0 {
		spec.Depth = []int{0}
	}
	if spec.Replicates <= 0 {
		spec.Replicates = 1
	}
	for _, m := range spec.Quirks {
		for _, k := range spec.Distractors {
			for _, pl := range spec.Plausibility {
				for _, pr := range spec.Prompt {
					for _, dp := range spec.Depth {
						for rep := range spec.Replicates {
							cell := GenSpec{
								Quirks: m, Distractors: k, Plausibility: pl,
								Prompt: pr, Depth: dp, Seed: spec.Seed, Replicate: rep,
							}
							row := SelectSweepRow{Cell: cell}
							dir, err := Generate(corpusRoot, cell)
							if err == nil {
								var traj *Trajectory
								traj, err = LoadTrajectory(dir)
								if err == nil {
									row.Report, err = r.RunSelectProbe(ctx, traj, dir, "", overlay)
								}
							}
							if err != nil {
								row.Error = err.Error()
							}
							if onRow != nil {
								onRow(row)
							}
							if ctx.Err() != nil {
								return ctx.Err()
							}
						}
					}
				}
			}
		}
	}
	return nil
}
