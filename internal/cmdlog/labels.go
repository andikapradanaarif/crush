// Artifact-based acceptance labels (#294, settled by #340): the
// episode verdict is the next user turn's cue-word judgment — a weak,
// language-bound signal. The label columns instead record what the
// artifacts show, under a fixed observation window: whether the
// edit's added lines still survive in the file, whether a commit
// made outside the harness touched the target inside the window,
// and whether the session's test runs ended green. Every signal
// keeps honest absence — NULL means never observed, never a passing
// grade. Once a row settles, its label freezes: the fold and the
// calibrator read only settled verdicts, so "certified on what it
// gates on" holds across time.
package cmdlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Settlement constants (#340).
const (
	// settleWindowMillis caps the observation window: a row settles
	// at the first of (a) the next session in the project starting or
	// (b) created_at + 24h — the label is computed once, then frozen.
	settleWindowMillis = int64(24 * 60 * 60 * 1000)
	// commitSkewMillis bounds agent-commit attribution: a commit
	// whose author time lands within this skew of a harness-run
	// `git commit`'s recorded last_at counts as agent-made and is
	// excluded from acceptance evidence.
	commitSkewMillis = int64(5000)
	// survivalMajority is the divergence cut: an edit counts as
	// diverged when strictly fewer than this fraction of its recorded
	// added lines remain in the file at settle time — line-level
	// survival so unrelated edits elsewhere in the file don't score
	// as revision.
	survivalMajority = 0.5
)

// ReferentEpisodeLabelInputs carries the label signals only the
// agent can compute — the judged turn's cost and scope violations
// come from the message trace, as does the added-lines fingerprint
// of the episode's edit. The file- and repo-side signals the store
// computes itself.
type ReferentEpisodeLabelInputs struct {
	// Steps is the tool-call count the judged turn issued — the
	// effort leg of "steps/tokens to done".
	Steps int64
	// Tokens is the session's accumulated request tokens through the
	// label pass — process-local, so a restarted session undercounts
	// rather than guesses.
	Tokens int64
	// WrongTarget is the mutating edits the judged turn produced
	// outside the admitted referent — the wrong-file signal. Always
	// zero today: the episode writer abstains on multi-target turns,
	// and the column is recorded so the signal exists the day that
	// rule relaxes.
	WrongTarget int64
	// AddedLines is the normalized fingerprint of the lines the
	// episode's edit introduced — the survival check's unit (#340).
	// Empty means no survival check can ever run for the episode.
	AddedLines []string
}

// LabelReferentEpisode stamps the artifact signals computable at
// episode-record time: the added-lines survival fingerprint, the
// judged turn's cost, the judged session's last-run test verdicts,
// and the observation window's outer edge. The update keys on the
// episode's (session, source message, target) unique constraint —
// the same idempotency point the insert uses, so a repair-chain
// re-derivation re-stamps rather than duplicates. Re-stamping also
// moves the fingerprint and the window: the referent is the latest
// derivation, not the first.
func (s *service) LabelReferentEpisode(ctx context.Context, ep ReferentEpisode, in ReferentEpisodeLabelInputs) error {
	added, _ := json.Marshal(in.AddedLines)
	now := s.now().UnixMilli()
	return s.q.LabelReferentEpisode(ctx, db.LabelReferentEpisodeParams{
		LabelTargetHash:  s.fileHash(ep.Target),
		LabelWrongTarget: in.WrongTarget,
		LabelSteps:       in.Steps,
		LabelTokens:      in.Tokens,
		LabelTestsGreen:  s.sessionTestsGreen(ctx, ep.SessionID),
		LabeledAt:        now,
		LabelAddedLines:  string(added),
		SettleAt:         now + settleWindowMillis,
		SessionID:        ep.SessionID,
		SourceMessageID:  ep.SourceMessageID,
		Target:           ep.Target,
	})
}

// SettleReferentLabels freezes the labels of episodes whose
// observation window closed (#340): a newer session started in the
// project, or created_at + 24h elapsed. For each due row it computes
// once — external-commit evidence inside [created_at, window_end]
// (commits the harness itself ran are excluded), hunk survival (the
// recorded added lines' presence in the file now), and the session's
// test verdict — then writes the settled mark. Rows recorded before
// settlement existed carry no added-lines fingerprint; they settle
// on commit and cue evidence alone — degraded, but honest.
//
// Bounded per call: every turn's pass drains the oldest due rows, so
// the backlog amortizes across turns without a scheduler. Outside a
// repository the commit signal stays NULL — honest absence.
func (s *service) SettleReferentLabels(ctx context.Context, limit int) error {
	// The pass fires detached every turn; two fast turns may overlap.
	// Writes are idempotent, so a skipped pass costs nothing.
	if !s.labelMu.TryLock() {
		return nil
	}
	defer s.labelMu.Unlock()
	if limit <= 0 {
		// LIMIT <= 0 in SQLite is unlimited — clamp, never unbound.
		limit = defaultListLimit
	}
	now := s.now().UnixMilli()
	rows, err := s.q.ListUnsettledReferentLabels(ctx, db.ListUnsettledReferentLabelsParams{
		ProjectKey: s.projectKey,
		SettleAt:   now,
		Limit:      int64(limit),
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	for _, r := range rows {
		if err := s.settleReferent(ctx, r, now); err != nil {
			return err
		}
	}
	return nil
}

// settleReferent computes and freezes one episode's label inside its
// closed window.
func (s *service) settleReferent(ctx context.Context, r db.ListUnsettledReferentLabelsRow, now int64) error {
	// Window end: the earlier of the +24h cap and the next session's
	// start. Rows predating the settle_at column carry 0 — treat the
	// cap as created_at + 24h so their window still has a real edge.
	cap := r.SettleAt
	if cap <= 0 {
		cap = r.CreatedAt + settleWindowMillis
	}
	winEnd := cap
	if next, err := s.q.NextSessionStartMillis(ctx, db.NextSessionStartMillisParams{
		CreatedAt: r.CreatedAt,
		ID:        r.SessionID,
	}); err == nil && next > 0 && next < winEnd {
		winEnd = next
	}

	var committed sql.NullInt64
	if s.hasRepo {
		committed = sql.NullInt64{Int64: 0, Valid: true}
		if s.externalCommitInWindow(ctx, []string{r.Target}, r.CreatedAt, winEnd) {
			committed.Int64 = 1
		}
	}
	changed := s.hunkDiverged(r.Target, r.LabelAddedLines)
	green := s.sessionTestsGreen(ctx, r.SessionID)
	return s.q.SettleReferentEpisode(ctx, db.SettleReferentEpisodeParams{
		LabelCommitted:   committed,
		LabelHashChanged: changed,
		LabelTestsGreen:  green,
		SettledAt:        sql.NullInt64{Int64: now, Valid: true},
		LabeledAt:        now,
		ID:               r.ID,
	})
}

// externalCommitInWindow reports whether any commit landing in
// (createdAt, winEnd] touched one of paths, excluding commits the
// harness itself made — command_memory records every `git commit`
// crush ran, and a commit whose author time sits within the skew
// bound of a recorded run is agent-made, not user acceptance.
// Timestamps mix units: git reports unix seconds, the ledger stores
// millis — all comparisons happen in millis.
func (s *service) externalCommitInWindow(ctx context.Context, paths []string, createdAt, winEnd int64) bool {
	commits := s.commitsTouchingPaths(ctx,
		time.UnixMilli(createdAt).UTC().Format(time.RFC3339),
		time.UnixMilli(winEnd).UTC().Format(time.RFC3339))
	if len(commits) == 0 {
		return false
	}
	runs, err := s.q.ListCrushCommitRuns(ctx, db.ListCrushCommitRunsParams{
		ProjectKey: s.projectKey,
		LastAt:     createdAt - commitSkewMillis,
		LastAt_2:   winEnd + commitSkewMillis,
	})
	if err != nil {
		runs = nil
	}
	for _, p := range paths {
		for _, ct := range commits[p] {
			ms := ct * 1000
			// Fail closed on edges: a commit we cannot prove
			// landed inside the window is not evidence, and a
			// commit matching a harness run is not external.
			if ms <= createdAt || ms > winEnd {
				continue
			}
			if isHarnessCommit(ms, runs) {
				continue
			}
			return true
		}
	}
	return false
}

// isHarnessCommit reports whether commit ms (unix millis) sits within
// the skew bound of a harness-recorded git-commit run.
func isHarnessCommit(ms int64, runs []int64) bool {
	for _, t := range runs {
		if ms >= t-commitSkewMillis && ms <= t+commitSkewMillis {
			return true
		}
	}
	return false
}

// hunkDiverged reads the survival signal (#340): the recorded added
// lines' presence in the file NOW. Diverged when the file is
// unreadable (deleted edits did not survive) or when fewer than
// survivalMajority of the fingerprint lines remain — unrelated edits
// elsewhere in the file no longer count as revision. An empty
// fingerprint keeps the signal NULL: rows predating the column and
// edits whose lines were never captured carry no survival evidence.
// Late settlement reads a later file state — a bounded bias toward
// revised, documented rather than hidden.
func (s *service) hunkDiverged(target, addedJSON string) sql.NullInt64 {
	var lines []string
	if addedJSON == "" || json.Unmarshal([]byte(addedJSON), &lines) != nil || len(lines) == 0 {
		return sql.NullInt64{}
	}
	data, ok := s.readWorkspaceFile(target)
	if !ok {
		// The file is gone — nothing survived.
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	content := "\n" + string(data) + "\n"
	present := 0
	for _, l := range lines {
		if strings.Contains(content, "\n"+l+"\n") {
			present++
		}
	}
	if float64(present)/float64(len(lines)) < survivalMajority {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

// NormalizeOutcomeCommand normalizes a bash invocation for the
// turn_outcomes commands column — the same whitespace-collapse,
// redaction, and length bound as the command_memory join key, so
// mined turn commands can join ledger rows directly.
func NormalizeOutcomeCommand(command string) string {
	return normalizeCommand(command)
}

// NormalizeAddedLines reduces an edit's introduced lines to the
// survival fingerprint: trimmed, empty-dropped, first-seen deduped,
// capped so a generated blob can't pin the row.
func NormalizeAddedLines(text string) []string {
	const maxLines = 256
	seen := map[string]struct{}{}
	out := make([]string, 0, 32)
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// fileHash is the legacy survival baseline: SHA-256 of the target's
// current content, workspace-relative. Still stamped at record time
// for diagnostics; settlement no longer reads it — whole-file
// equality counted unrelated edits as divergence (#340). Empty when
// the file is unreadable. Paths escaping the workspace refuse.
func (s *service) fileHash(target string) string {
	if target == "" {
		return ""
	}
	data, ok := s.readWorkspaceFile(target)
	if !ok {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readWorkspaceFile reads a workspace-relative path, refusing
// escapes — label targets come from tool-call paths, and a stored
// "../x" must not read outside.
func (s *service) readWorkspaceFile(target string) ([]byte, bool) {
	if target == "" {
		return nil, false
	}
	clean := filepath.Clean(filepath.FromSlash(target))
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(s.workingDir, clean))
	if err != nil {
		return nil, false
	}
	return data, true
}

// sessionTestsGreen folds the judged session's last-run test
// verdicts into the tri-state signal: every observed kind='test'
// command ending clean is 1, any failure is 0, and a session that
// ran nothing the ledger calls a test leaves NULL — unobserved, not
// green. The ledger's last-exit semantics mean a test the session
// ran early still counts toward session-end state.
func (s *service) sessionTestsGreen(ctx context.Context, sessionID string) sql.NullInt64 {
	rows, err := s.q.ListSessionTestVerdicts(ctx, db.ListSessionTestVerdictsParams{
		ProjectKey:      s.projectKey,
		ID:              sessionID,
		ParentSessionID: sql.NullString{String: sessionID, Valid: true},
	})
	if err != nil || len(rows) == 0 {
		return sql.NullInt64{}
	}
	for _, r := range rows {
		if r.LastExit != 0 {
			return sql.NullInt64{Int64: 0, Valid: true}
		}
	}
	return sql.NullInt64{Int64: 1, Valid: true}
}

// commitsTouchingPaths maps each path a commit in the window touched
// to that commit's unix-second timestamps — one git call for the
// whole batch, bounded both ends (#340). Commit headers are
// NUL-prefixed "\x00commit\t<sec>": filenames cannot contain NUL, so
// a path literally named "commit <digits>" can never fake a boundary.
func (s *service) commitsTouchingPaths(ctx context.Context, sinceRFC3339, untilRFC3339 string) map[string][]int64 {
	out := map[string][]int64{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := gitOut(ctx, s.workingDir, "log",
		"--since="+sinceRFC3339, "--until="+untilRFC3339,
		"--format=%x00commit%x09%ct", "--name-only")
	if err != nil {
		return out
	}
	var ct int64 = -1
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "\x00commit\x09"):
			if n, perr := strconv.ParseInt(strings.TrimPrefix(line, "\x00commit\x09"), 10, 64); perr == nil {
				ct = n
			} else {
				ct = -1
			}
		case line != "" && ct >= 0:
			out[line] = append(out[line], ct)
		}
	}
	return out
}
