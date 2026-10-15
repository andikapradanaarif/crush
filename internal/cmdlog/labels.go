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
	// whose committer time (%ct — the right clock for evidence
	// windows; author time can backdate) lands within this skew of a
	// harness-run `git commit`'s recorded last_at counts as
	// agent-made and is excluded from acceptance evidence.
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
// moves the fingerprint and the window — the outer edge runs from
// label time, so a re-derivation widens the commit window beyond the
// episode's created_at + 24h. Intentional: the observation covers
// the latest derivation of the referent, not the superseded one.
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
	// Commit evidence is prefetched for the whole batch: one git log
	// over the union window plus one ledger read — not one exec per
	// row. Each row still filters inside its own (created_at, winEnd].
	winLo, winHi := now, int64(0)
	winEnds := make([]int64, len(rows))
	for i, r := range rows {
		winEnds[i] = s.windowEnd(ctx, r.CreatedAt, r.SessionID, r.SettleAt)
		if r.CreatedAt < winLo {
			winLo = r.CreatedAt
		}
		if winEnds[i] > winHi {
			winHi = winEnds[i]
		}
	}
	ev := s.prefetchCommitEvidence(ctx, winLo, winHi)
	for i, r := range rows {
		if err := s.settleReferent(ctx, r, winEnds[i], ev, now); err != nil {
			return err
		}
	}
	return nil
}

// windowEnd computes a row's inner window edge — the earlier of the
// +24h cap and the next provable session's start. Rows predating the
// settle_at column carry 0; the cap reconstructs as created_at+24h.
func (s *service) windowEnd(ctx context.Context, createdAt int64, sessionID string, settleAt int64) int64 {
	cap := settleAt
	if cap <= 0 {
		cap = createdAt + settleWindowMillis
	}
	if next, err := s.q.NextSessionStartMillis(ctx, db.NextSessionStartMillisParams{
		CreatedAt:    createdAt,
		ID:           sessionID,
		ProjectKey:   s.projectKey,
		ProjectKey_2: s.projectKey,
		ProjectKey_3: s.projectKey,
	}); err == nil && next > 0 && next < cap {
		return next
	}
	return cap
}

// commitEvidence is one settle batch's prefetched commit data: every
// commit touching paths inside the union window, plus the
// harness-recorded commit runs to exclude from it.
type commitEvidence struct {
	commits map[string][]int64
	runs    []int64
	// runsUnreadable marks the harness-run exclusion list as
	// degraded: without it an agent-run commit is indistinguishable
	// from external acceptance, so the commit arm abstains — the
	// same fail-closed direction the commits fetch takes.
	runsUnreadable bool
}

// prefetchCommitEvidence fetches the batch's two evidence inputs in
// one git call and one ledger read. Outside a repo both are empty —
// every inWindow read is then simply false. A ledger read failure
// marks the batch degraded rather than treating every commit as
// external: the exclusion list is what keeps agent-made commits out
// of acceptance evidence.
func (s *service) prefetchCommitEvidence(ctx context.Context, winLo, winHi int64) commitEvidence {
	ev := commitEvidence{}
	if !s.hasRepo || winHi <= winLo {
		return ev
	}
	ev.commits = s.commitsTouchingPaths(ctx,
		time.UnixMilli(winLo).UTC().Format(time.RFC3339),
		time.UnixMilli(winHi).UTC().Format(time.RFC3339))
	runs, err := s.q.ListCrushCommitRuns(ctx, db.ListCrushCommitRunsParams{
		ProjectKey: s.projectKey,
		LastAt:     winLo - commitSkewMillis,
		LastAt_2:   winHi + commitSkewMillis,
	})
	if err != nil {
		ev.runsUnreadable = true
	} else {
		ev.runs = runs
	}
	return ev
}

// inWindow reports whether any prefetched commit landing in
// (createdAt, winEnd] touched one of paths, excluding commits the
// harness itself made — a commit whose time sits within the skew
// bound of a recorded run is agent-made, not user acceptance.
// Timestamps mix units: git reports unix seconds, the ledger stores
// millis — all comparisons happen in millis.
func (ev commitEvidence) inWindow(paths []string, createdAt, winEnd int64) bool {
	if ev.runsUnreadable {
		// No exclusion list means no way to prove a commit was
		// external — abstain rather than count agent work as
		// acceptance.
		return false
	}
	for _, p := range paths {
		// Stored targets are slash-normalized at record time; the
		// ToSlash here covers rows written before that (Windows
		// filepath.Rel emitted backslashes) — git paths are always
		// slash-separated.
		for _, ct := range ev.commits[filepath.ToSlash(p)] {
			ms := ct * 1000
			// Fail closed on edges: a commit we cannot prove
			// landed inside the window is not evidence, and a
			// commit matching a harness run is not external.
			if ms <= createdAt || ms > winEnd {
				continue
			}
			if isHarnessCommit(ms, ev.runs) {
				continue
			}
			return true
		}
	}
	return false
}

// settleReferent computes and freezes one episode's label inside its
// closed window.
func (s *service) settleReferent(ctx context.Context, r db.ListUnsettledReferentLabelsRow, winEnd int64, ev commitEvidence, now int64) error {
	var committed sql.NullInt64
	if s.hasRepo {
		committed = sql.NullInt64{Int64: 0, Valid: true}
		if ev.inWindow([]string{r.Target}, r.CreatedAt, winEnd) {
			committed.Int64 = 1
		}
	}
	changed := s.hunkDiverged(r.Target, r.LabelAddedLines)
	green := s.sessionTestsGreen(ctx, r.SessionID)
	if err := s.q.SettleReferentEpisode(ctx, db.SettleReferentEpisodeParams{
		LabelCommitted:   committed,
		LabelHashChanged: changed,
		LabelTestsGreen:  green,
		SettledAt:        sql.NullInt64{Int64: now, Valid: true},
		LabeledAt:        now,
		ID:               r.ID,
	}); err != nil {
		return err
	}
	// Promotion consumes settled outcomes (#340): this row's success
	// freezing may be the crossing observation for its mapping, so
	// the floor check runs here too — the record path can't count the
	// episode it just inserted. Suggested rows are echo, not
	// evidence; zero/negative floor disables the channel.
	if s.referentPromoteHits > 0 && r.MemorySuggested == 0 &&
		ReferentEpisodeOutcome(r.Verdict, committed, changed) {
		return s.maybePromoteReferent(ctx, r.Phrase, r.Target, s.referentPromoteHits)
	}
	return nil
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
//
// Both sides compare in NormalizeAddedLines' space — trimmed,
// empty-dropped, deduped — because the fingerprint stores trimmed
// text while files carry indentation: probing raw content for
// "\n"+line+"\n" only ever matches column-0 lines and would read
// nearly every real edit as diverged.
func (s *service) hunkDiverged(target, addedJSON string) sql.NullInt64 {
	var lines []string
	if addedJSON == "" || json.Unmarshal([]byte(addedJSON), &lines) != nil || len(lines) == 0 {
		return sql.NullInt64{}
	}
	if !workspacePathSafe(target) {
		// An absolute or escaping path is out of scope, not deleted —
		// abstain rather than minting diverged evidence for an edit
		// outside this workspace.
		return sql.NullInt64{}
	}
	data, ok := s.readWorkspaceFile(target)
	if !ok {
		// The file is gone — nothing survived.
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	fileLines := map[string]struct{}{}
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			fileLines[t] = struct{}{}
		}
	}
	present := 0
	for _, l := range lines {
		if _, ok := fileLines[l]; ok {
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

// workspacePathSafe reports whether a stored target stays inside the
// workspace — absolute and ../-escaping paths are refused. Settlement
// distinguishes this from "file deleted": an out-of-scope path
// abstains (NULL) rather than reading as diverged.
func workspacePathSafe(target string) bool {
	if target == "" {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(target))
	return !filepath.IsAbs(clean) && clean != ".." &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// readWorkspaceFile reads a workspace-relative path, refusing
// escapes — label targets come from tool-call paths, and a stored
// "../x" must not read outside.
func (s *service) readWorkspaceFile(target string) ([]byte, bool) {
	if !workspacePathSafe(target) {
		return nil, false
	}
	clean := filepath.Clean(filepath.FromSlash(target))
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
// a path literally named "commit <digits>" can never fake a
// boundary. git emits repo-root-relative paths while ledger targets
// are workspace-relative — repoPrefix strips the prefix and drops
// commits outside the workspace entirely (without it, commit
// evidence is silently blind in a repo subdirectory). Known miss:
// git C-quotes paths with tabs/newlines/non-ASCII (core.quotePath)
// while the ledger stores them raw, so commit evidence silently
// misses for such files — a `-c core.quotePath=false -z` read is the
// fix if the class ever matters.
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
			if s.repoPrefix != "" {
				if !strings.HasPrefix(line, s.repoPrefix) {
					continue // committed path outside this workspace
				}
				line = strings.TrimPrefix(line, s.repoPrefix)
			}
			out[line] = append(out[line], ct)
		}
	}
	return out
}
