package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
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
// seedKey is the trajectory's pinned partition key — the seeded
// rows must land under the key the measured run's project_key
// option resolves, or the measured session reads an empty store.
//
// The returned ids are the seed session ids, in seed order — the
// caller folds them into the warm-start ledger for provenance.
func (r *Runner) runScriptedSeeds(ctx context.Context, workdir string, seeds []ScriptedSeed, seedKey string) ([]string, error) {
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
		sessionID, err := r.runScriptedSeed(ctx, q, conn, workdir, seed, i, seedKey)
		if err != nil {
			return sessionIDs, fmt.Errorf("seed_commands[%d]: %w", i, err)
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	// Materialize session digests eagerly: the production write path
	// is lazy (first retrieval turn), which would move a db write —
	// and its nondeterminism — into the measured run and out of the
	// snapshot. Refreshing here means the snapshot carries the digest
	// rows and the gate can count them. The runner refreshes again
	// after agent seeds so mixed trajectories land digests for every
	// seed kind before the gate. A refresh failure is logged, not
	// fatal — lazy materialization stays correct and the seed gate
	// still enforces the designed state.
	svc := cmdlog.NewService(q, workdir, params.DefaultMemory(),
		cmdlog.WithProjectKey(seedKey))
	if err := svc.RefreshSessionDigests(ctx, params.DefaultMemory().DigestRefreshLimit); err != nil {
		slog.Warn("Seeded digest refresh failed — digests will materialize lazily",
			"error", err)
	}
	return sessionIDs, nil
}

// materializeSeededDigests opens the run db and refreshes session
// digest rows for every stale session — the runner calls it after
// all seed kinds so agent-seeded sessions' digests land before the
// gate and inside the snapshot, where runScriptedSeeds' own refresh
// (scripted seeds only) already left the scripted rows materialized.
func (r *Runner) materializeSeededDigests(ctx context.Context, workdir, seedKey string) error {
	dataDir := DataDirFor(workdir)
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("digest refresh db: %w", err)
	}
	defer func() {
		if err := db.Release(dataDir); err != nil {
			slog.Warn("Failed to release digest refresh db", "error", err)
		}
	}()
	svc := cmdlog.NewService(db.New(conn), workdir, params.DefaultMemory(),
		cmdlog.WithProjectKey(seedKey))
	return svc.RefreshSessionDigests(ctx, params.DefaultMemory().DigestRefreshLimit)
}

// runScriptedSeed runs one seed session: a session row backdated to
// the seed's clock, then each command through shell.RunAndCapture +
// RecordRun under that clock.
func (r *Runner) runScriptedSeed(ctx context.Context, q *db.Queries, conn *sql.DB, workdir string, seed ScriptedSeed, ord int, seedKey string) (string, error) {
	sessionID := uuid.New().String()
	// The session row is backdated like the command rows it owns —
	// audit reading "seeded 3d ago" holds for the session too.
	stamp := r.now().Add(-time.Duration(seed.AgoSeconds * float64(time.Second)))
	title := seed.Title
	if title == "" {
		title = fmt.Sprintf("seed %d (scripted)", ord+1)
	}
	if _, err := q.CreateSession(ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: title,
	}); err != nil {
		return "", fmt.Errorf("create seed session: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE sessions SET created_at = ?, updated_at = ? WHERE id = ?`,
		stamp.Unix(), stamp.Unix(), sessionID); err != nil {
		return "", fmt.Errorf("backdate seed session: %w", err)
	}

	svc := cmdlog.NewService(q, workdir, params.DefaultMemory(),
		cmdlog.WithClock(func() time.Time { return stamp }),
		cmdlog.WithProjectKey(seedKey))
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
	// Files seed the session's touched paths through the same
	// filetracker write a view/edit call would take — they are what
	// the session digest's file hints are built from. The rows'
	// read_at is SQL-stamped at write time, so backdate it to the
	// seed's clock like the session row above.
	if len(seed.Files) > 0 {
		ft := filetracker.NewService(q, workdir)
		for _, f := range seed.Files {
			ft.RecordRead(ctx, sessionID, f)
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE read_files SET read_at = ? WHERE session_id = ?`,
			stamp.Unix(), sessionID); err != nil {
			return sessionID, fmt.Errorf("backdate seed file reads: %w", err)
		}
	}
	// Referent episodes record through the real judged-episode write
	// path under the seed's clock — created_at ages with the session.
	// Accepted, unsuggested episodes from distinct seed sessions
	// compose toward the promotion floor exactly like live usage.
	for i, ref := range seed.Referents {
		verdict := cmdlog.ReferentAccepted
		if ref.Revised {
			verdict = cmdlog.ReferentRevised
		}
		if err := svc.RecordReferentEpisode(ctx, cmdlog.ReferentEpisode{
			Phrase:          ref.Phrase,
			Target:          ref.Target,
			SessionID:       sessionID,
			SourceMessageID: fmt.Sprintf("seed-%d-ref-%d", ord, i),
			ToolCallID:      fmt.Sprintf("seed-%d-refcall-%d", ord, i),
			Verdict:         verdict,
		}, params.DefaultMemory().ReferentPromoteHits); err != nil {
			return sessionID, fmt.Errorf("referent %d (%q→%q): %w", i, ref.Phrase, ref.Target, err)
		}
	}
	return sessionID, nil
}

// --- seed snapshots ---
//
// A seeded trajectory's warm state is deterministic enough to reuse:
// once scripted seeds (and any agent seeds) have run and the gate
// passed, the workdir + crush.db snapshot becomes the cell's starting
// state for every later attempt and arm. Seed variance leaves the
// measured estimate entirely — the replay restores bytes, so what
// differs between two runs of a cell is only the measured session.
// project_key is pinned (eval-<content hash>) rather than
// path-derived, so a DB restored under a different workdir path still
// resolves the partition its rows were written with.

// seedSnapshotMeta is the warm ledger a replayed run reports — the
// seeding cost and session provenance of the ORIGINAL seed, not the
// restore's near-zero cost.
type seedSnapshotMeta struct {
	SessionIDs []string        `json:"session_ids"`
	Sessions   int             `json:"sessions"`
	DurationS  float64         `json:"duration_s"`
	Steps      int             `json:"steps"`
	Tokens     TokenUsage      `json:"tokens"`
	Generator  GeneratorTokens `json:"generator_tokens"`
}

// snapshotDir locates the trajectory's seed snapshot beside the
// materialized workdirs — same lifetime as the run's scratch space.
func (r *Runner) snapshotDir(key string) string {
	return filepath.Join(r.workParent(), ".snapshots", key)
}

// restoreSnapshot lays a prior seed snapshot over the materialized
// workdir and returns its ledger. false means no snapshot exists —
// the caller seeds fresh; a restore error falls back the same way
// since the seeds, not the snapshot, are the source of truth.
func (r *Runner) restoreSnapshot(key, workdir string, warm *WarmStart) bool {
	dir := r.snapshotDir(key)
	metaRaw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return false
	}
	var meta seedSnapshotMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		slog.Warn("Seed snapshot meta unreadable — re-seeding", "snapshot", dir, "error", err)
		return false
	}
	if err := overlaySnapshot(dir, workdir); err != nil {
		slog.Warn("Seed snapshot restore failed — re-seeding", "error", err)
		return false
	}
	// Provenance (session ids, count) carries — the seed rows in the
	// restored db belong to those sessions. Spend does not: the
	// seeding tokens/steps were paid once by the seeding attempt,
	// and replaying the ledger into every run would price the seed
	// N times over. A replayed warm_start reads zero cost with
	// real session ids — "seeded once, measured N" in the ledger.
	warm.SessionIDs = meta.SessionIDs
	warm.Sessions = meta.Sessions
	return true
}

// overlaySnapshot lays a snapshot's workdir tree and crush.db over a
// materialized workdir. The tree is cleared first — copyTree adds and
// overwrites but never deletes, so a file absent at snapshot time
// would resurrect from the fixture copy and break byte-identity.
func overlaySnapshot(dir, workdir string) error {
	dataDir := DataDirFor(workdir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(workdir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(workdir, e.Name())); err != nil {
			return err
		}
	}
	if err := copyTree(filepath.Join(dir, "workdir"), workdir); err != nil {
		return err
	}
	dbBytes, err := os.ReadFile(filepath.Join(dir, "crush.db"))
	if err != nil {
		return err
	}
	// Any WAL sidecar left over from this attempt's own db init
	// would replay over the restored bytes — the snapshot's
	// checkpointed file is the complete state.
	for _, side := range []string{"crush.db-wal", "crush.db-shm"} {
		_ = os.Remove(filepath.Join(dataDir, side))
	}
	return os.WriteFile(filepath.Join(dataDir, "crush.db"), dbBytes, 0o600)
}

// captureDB returns a self-contained copy of the workdir's crush.db:
// checkpointed when the db re-opens cleanly, raw bytes otherwise.
// Re-opening a db the write path just used can fail on Windows
// (SQLITE_NOTADB on a file that just proved valid) — the last conn
// close already checkpointed the WAL, so the raw file is complete.
// ok=false means no usable db exists at all.
func captureDB(ctx context.Context, dataDir string) ([]byte, bool) {
	conn, err := db.Connect(ctx, dataDir)
	if err == nil {
		// Checkpoint so the copy is self-contained — the bytes alone
		// must carry every committed row.
		_, _ = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		_ = db.Release(dataDir)
	}
	dbBytes, rerr := os.ReadFile(filepath.Join(dataDir, "crush.db"))
	switch {
	case rerr != nil:
		slog.Warn("Snapshot db capture failed", "error", firstErr(err, rerr))
		return nil, false
	case err != nil:
		slog.Warn("Snapshot checkpoint skipped — copying db raw", "error", err)
	}
	return dbBytes, true
}

// writeSnapshot captures the post-seed state for later attempts. The
// db is checkpointed first so the file copy carries the WAL too.
// Failures are logged, not fatal — snapshotting is a variance
// optimization, the next attempt can always seed fresh.
func (r *Runner) writeSnapshot(ctx context.Context, key, workdir string, warm *WarmStart) {
	dir := r.snapshotDir(key)
	tmp := dir + ".tmp-" + uuid.New().String()
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "workdir"), 0o755); err != nil {
		slog.Warn("Seed snapshot mkdir failed", "error", err)
		return
	}
	dataDir := DataDirFor(workdir)
	dbBytes, ok := captureDB(ctx, dataDir)
	if !ok {
		return
	}
	if err := os.WriteFile(filepath.Join(tmp, "crush.db"), dbBytes, 0o600); err != nil {
		return
	}
	if err := copyTree(workdir, filepath.Join(tmp, "workdir")); err != nil {
		slog.Warn("Seed snapshot workdir capture failed", "error", err)
		return
	}
	meta := seedSnapshotMeta{
		SessionIDs: warm.SessionIDs,
		Sessions:   warm.Sessions,
		DurationS:  warm.DurationS,
		Steps:      warm.Steps,
		Tokens:     warm.Tokens,
		Generator:  warm.GeneratorTokens,
	}
	if raw, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(filepath.Join(tmp, "meta.json"), raw, 0o644)
	}
	// Atomic publish: a partial snapshot is never visible to a
	// parallel attempt — rename is all-or-nothing per platform.
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		slog.Warn("Seed snapshot publish failed", "error", err)
	}
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
