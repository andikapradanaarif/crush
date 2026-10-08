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
	// Reuse a seeded workdir — the presence of a crush.db means the
	// seeds landed. A dir without one was a torn seed attempt;
	// re-materialize rather than trust a partial state.
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			cand := filepath.Join(parent, e.Name())
			if e.IsDir() && fileExists(filepath.Join(DataDirFor(cand), "crush.db")) {
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
	return workdir, seedKey, nil
}

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
