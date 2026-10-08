package eval

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/charmbracelet/crush/internal/shell"
)

// runScriptedSeeds executes trajectory.seed_commands: each element is
// one seed session — a real sessions row, real shell-interpreter
// execution, real RecordRun writes — stamped under a clock backdated
// by ago_seconds. The seeds produce the same command_memory and
// failure_memory rows an agent's bash calls would, minus the tokens
// and the variance: dose is authored, so the measured session sees
// the designed history rather than whatever a seed agent happened to
// leave.
//
// The returned ids are the seed session ids, in seed order — the
// caller folds them into the warm-start ledger for provenance.
func (r *Runner) runScriptedSeeds(ctx context.Context, workdir string, seeds []ScriptedSeed) ([]string, error) {
	dataDir := DataDirFor(workdir)
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return nil, fmt.Errorf("seed db: %w", err)
	}
	defer func() {
		if err := db.Release(dataDir); err != nil {
			slog.Warn("Failed to release seed db", "error", err)
		}
	}()
	q := db.New(conn)

	sessionIDs := make([]string, 0, len(seeds))
	for i, seed := range seeds {
		sessionID, err := r.runScriptedSeed(ctx, q, conn, workdir, seed, i)
		if err != nil {
			return sessionIDs, fmt.Errorf("seed_commands[%d]: %w", i, err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	return sessionIDs, nil
}

// runScriptedSeed runs one seed session: a session row backdated to
// the seed's clock, then each command through shell.RunAndCapture +
// RecordRun under that clock.
func (r *Runner) runScriptedSeed(ctx context.Context, q *db.Queries, conn *sql.DB, workdir string, seed ScriptedSeed, ord int) (string, error) {
	sessionID := uuid.New().String()
	// The session row is backdated like the command rows it owns —
	// audit reading "seeded 3d ago" holds for the session too.
	stamp := r.now().Add(-time.Duration(seed.AgoSeconds * float64(time.Second)))
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: fmt.Sprintf("seed %d (scripted)", ord+1),
	}); err != nil {
		return "", fmt.Errorf("create seed session: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE sessions SET created_at = ?, updated_at = ? WHERE id = ?`,
		stamp.Unix(), stamp.Unix(), sessionID); err != nil {
		return "", fmt.Errorf("backdate seed session: %w", err)
	}

	svc := cmdlog.NewService(q, workdir, params.DefaultMemory(), cmdlog.WithClock(
		func() time.Time { return stamp }))
	for j, command := range seed.Commands {
		res, err := shell.RunAndCapture(ctx, shell.RunOptions{
			Command: command,
			Cwd:     workdir,
			Env:     r.checkEnv(),
		})
		if err != nil {
			return sessionID, fmt.Errorf("command %d %q: %w", j, command, err)
		}
		if !res.Verdict {
			// No exit status — parse error or denial. The authored
			// state diverged from the script: error like an agent
			// seed that died, don't measure a different cell.
			return sessionID, fmt.Errorf("command %d %q reached no verdict (exit %d)", j, command, res.ExitCode)
		}
		// RunAndCapture merges stdout+stderr into Output; it lands
		// on Stdout — failureHeadline scans stdout after stderr,
		// so verdict lines still surface as headlines.
		svc.RecordRun(ctx, cmdlog.Run{
			SessionID:      sessionID,
			ToolCallID:     fmt.Sprintf("seed-%d-%d", ord, j),
			Command:        command,
			CWD:            workdir,
			Stdout:         res.Output,
			ExitCode:       res.ExitCode,
			Ran:            res.Verdict,
			ComponentExits: res.ComponentExits,
		})
	}
	return sessionID, nil
}
