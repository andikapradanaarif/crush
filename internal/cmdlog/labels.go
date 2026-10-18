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
// re-derivation re-stamps rather than duplicates. Re-stamping moves
// the fingerprint but not the cap: the SQL adds the window to the
// row's own created_at, so a label landing late — a repair-chain
// retry — cannot widen the documented created_at + 24h edge (#354).
// A very late re-derivation therefore settles against the original
// window's evidence rather than minting a fresh one.
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
		// Named for the column it offsets in SQL (created_at + ?);
		// the value is the window duration, not a timestamp.
		CreatedAt:       settleWindowMillis,
		SessionID:       ep.SessionID,
		SourceMessageID: ep.SourceMessageID,
		Target:          ep.Target,
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

// commitRef is one commit inside the observation window: its object
// id for crush-made exclusion and its committer time (%ct — the
// right clock for evidence windows; author time can backdate).
// ct is unix SECONDS — windows and ledger stamps are millis.
type commitRef struct {
	sha string
	ct  int64
}

// commitEvidence is one settle batch's prefetched commit data: every
// commit touching paths inside the union window, plus the exact SHAs
// the harness itself produced to exclude from it.
type commitEvidence struct {
	commits map[string][]commitRef
	made    map[string]struct{}
	// exclusionUnreadable marks the crush-made list as degraded:
	// without it an agent-run commit is indistinguishable from
	// external acceptance, so the commit arm abstains — the same
	// fail-closed direction the commits fetch takes.
	exclusionUnreadable bool
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
	// Exclusion is an exact SHA match against commits the harness's
	// own runs produced — no spelling patterns, no time skew: every
	// producer (rebase, merge, cherry-pick, am, commit-tree, sudo,
	// chained forms) attributes the same way at record time (#359).
	shas, err := s.q.ListCrushCommitSHAs(ctx, s.projectKey)
	if err != nil {
		ev.exclusionUnreadable = true
	} else {
		ev.made = map[string]struct{}{}
		for _, sha := range shas {
			ev.made[sha] = struct{}{}
		}
	}
	return ev
}

// inWindow reports whether any prefetched commit landing in
// (createdAt, winEnd] touched one of paths, excluding commits the
// harness itself made — an SHA recorded in crush_commits is
// agent-made, not user acceptance.
// Timestamps mix units: git reports unix seconds, the ledger stores
// millis — all comparisons happen in millis.
func (ev commitEvidence) inWindow(paths []string, createdAt, winEnd int64) bool {
	if ev.exclusionUnreadable {
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
		for _, c := range ev.commits[filepath.ToSlash(p)] {
			ms := c.ct * 1000
			// Fail closed on edges: a commit we cannot prove
			// landed inside the window is not evidence, and a
			// commit the harness made is not external.
			if ms <= createdAt || ms > winEnd {
				continue
			}
			if _, ok := ev.made[c.sha]; ok {
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
		if s.pathEscapesWorkspace(target) {
			// A symlink resolving outside the workspace abstains like
			// a textual escape — the target exists but is out of
			// scope, not deleted (#400).
			return sql.NullInt64{}
		}
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
// "../x" must not read outside. os.Root closes the textual check's
// symlink hole: a link inside the workspace pointing out now fails
// the read race-free (#400).
func (s *service) readWorkspaceFile(target string) ([]byte, bool) {
	if !workspacePathSafe(target) {
		return nil, false
	}
	root, err := os.OpenRoot(s.workingDir)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	data, err := root.ReadFile(filepath.FromSlash(target))
	if err != nil {
		return nil, false
	}
	return data, true
}

// pathEscapesWorkspace reports whether target exists but resolves —
// through any symlink in its path — to a location outside the
// workspace. Settlement treats that like a textual escape (abstain),
// not a deletion. Best-effort: an unresolvable path (missing,
// permission) reports false and the read failure decides.
func (s *service) pathEscapesWorkspace(target string) bool {
	resolved, err := filepath.EvalSymlinks(
		filepath.Join(s.workingDir, filepath.FromSlash(target)))
	if err != nil {
		return false
	}
	base, err := filepath.EvalSymlinks(s.workingDir)
	if err != nil {
		return false
	}
	return resolved != base &&
		!strings.HasPrefix(resolved, base+string(filepath.Separator))
}

// sessionTestsGreen folds the judged session's last-run test
// verdicts into the tri-state signal: every observed kind='test'
// command ending clean is 1, any failure is 0, and a session that
// ran nothing the ledger calls a test leaves NULL — unobserved, not
// green. Evidence comes from the session tree's own run history
// (session_command_runs — immune to the ledger's last-writer drift,
// #357) unioned with last-writer command_memory rows the session
// still owns, the only source on stores predating that table.
// Within the tree the LATEST verdict per command wins — a child's
// red lifts when the parent re-runs it green — while a foreign
// session's re-run can never erase the tree's evidence. The
// last-exit semantics mean a test the session ran early still
// counts toward session-end state — session-grain by design (#353).
func (s *service) sessionTestsGreen(ctx context.Context, sessionID string) sql.NullInt64 {
	rows, err := s.q.ListSessionTestVerdicts(ctx, db.ListSessionTestVerdictsParams{
		ProjectKey: s.projectKey,
		SessionID:  sessionID,
	})
	if err != nil || len(rows) == 0 {
		return sql.NullInt64{}
	}
	for _, e := range rows {
		if e != 0 {
			return sql.NullInt64{Int64: 0, Valid: true}
		}
	}
	return sql.NullInt64{Int64: 1, Valid: true}
}

// commitsTouchingPaths maps each path a commit in the window touched
// to the refs of the commits that touched it — {sha, unix-second
// committer time} — one git call for the whole batch, bounded both
// ends (#340). Commit headers are NUL-prefixed
// "\x00commit\t<sec>\t<sha>": filenames cannot contain NUL, so a
// path literally named "commit <digits>" can never fake a boundary.
// git emits repo-root-relative paths while ledger targets are
// workspace-relative — repoPrefix strips the prefix and drops
// commits outside the workspace entirely (without it, commit
// evidence is silently blind in a repo subdirectory). --all covers
// commits on every reachable ref, not just the checked-out HEAD, so
// a user commit made on another branch inside the window still
// counts (#360) — bounded the same way by since/until. Known
// misses: worktrees sharing one data_directory still read evidence
// from whichever checkout a worktree's HEAD points at (partition-
// vs-checkout mismatch is a design problem, not a patch); and git
// C-quotes paths with tabs/newlines/non-ASCII (core.quotePath)
// while the ledger stores them raw, so commit evidence silently
// misses for such files — a `-c core.quotePath=false -z` read is the
// fix if the class ever matters.
func (s *service) commitsTouchingPaths(ctx context.Context, sinceRFC3339, untilRFC3339 string) map[string][]commitRef {
	out := map[string][]commitRef{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := gitOut(ctx, s.workingDir, "log", "--all",
		"--since="+sinceRFC3339, "--until="+untilRFC3339,
		"--format=%x00commit%x09%ct%x09%H", "--name-only")
	if err != nil {
		return out
	}
	var cur commitRef
	var have bool
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "\x00commit\x09"):
			fields := strings.Split(strings.TrimPrefix(line, "\x00commit\x09"), "\t")
			if len(fields) == 2 {
				if n, perr := strconv.ParseInt(fields[0], 10, 64); perr == nil {
					cur, have = commitRef{sha: fields[1], ct: n}, true
					continue
				}
			}
			have = false
		case line != "" && have:
			if s.repoPrefix != "" {
				if !strings.HasPrefix(line, s.repoPrefix) {
					continue // committed path outside this workspace
				}
				line = strings.TrimPrefix(line, s.repoPrefix)
			}
			out[line] = append(out[line], cur)
		}
	}
	return out
}
