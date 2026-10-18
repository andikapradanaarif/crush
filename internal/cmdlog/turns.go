// Turn-level outcome labels (#340): the acceptance verdict at the
// unit procedural memory mines — one row per (session, user turn),
// carrying what the turn did (edited paths, added-line fingerprints,
// commands) and what the follow-up judged (next_verdict). Rows open
// unsettled and freeze once at their observation window's close —
// the same settlement contract the referent labels follow, so the
// fold and the calibrator never read a label that can still change.
package cmdlog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/redact"
)

// Turn outcome verdicts — the closed vocabulary settled rows carry.
// unknown covers every case the evidence cannot decide: read-only
// turns, unreadable files with no commit evidence, unparseable
// follow-ups. It is never a passing grade.
const (
	TurnAccepted = "accepted"
	TurnRevised  = "revised"
	TurnFailed   = "failed"
	TurnUnknown  = "unknown"
)

// HunkEdit is one applied mutation's verbatim input pair (#394):
// the text it replaced and the text it wrote. old both preserves the
// pre-edit content and helps locate the region later; a write call
// contributes {new: content}, the old file being unknowable from the
// call. Entries stay per-edit so same-turn self-corrections don't
// blur together — a write-then-edit sequence must not read the
// write's text as still-live output.
type HunkEdit struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// TurnOutcome is what one user turn did — the row's evidence columns.
// The agent extracts it from the message slice (or stored parts on
// backfill); the store owns settlement.
type TurnOutcome struct {
	SessionID string
	// TurnID is the turn's user-message ID — the row's identity.
	// Stable under compaction, where a slice-relative ordinal would
	// shift between the live tail and the full-transcript backfill.
	TurnID string
	// EditedPaths are the workspace-relative files the turn mutated.
	EditedPaths []string
	// EditHunks maps each edited path to the normalized added lines
	// its edits introduced — the survival fingerprint.
	EditHunks map[string][]string
	// HunkAdded maps each edited path to the turn's verbatim edits
	// there (#394) — the agent-output half of the (output,
	// what-the-region-became) pair the preference tier mines. The
	// store bounds, redacts, and locates spans at write.
	HunkAdded map[string][]HunkEdit
	// Commands are the turn's bash invocations in ledger-normal form
	// (NormalizeOutcomeCommand).
	Commands []string
	// NextVerdict is the cue judgment the following user prompt
	// carried at record time — "" when the turn closed without a
	// follow-up (session end) or the backfill observed none.
	NextVerdict string
	// CreatedAt is the turn's user-message timestamp in millis. Zero
	// falls back to record time; the backfill stamps the real one so
	// historical rows keep their true observation window and commit
	// evidence inside it can still count.
	CreatedAt int64
}

// RecordTurnOutcome opens one turn's row on (session_id, turn_id).
// settle_at is the +24h cap off the turn's own timestamp; the
// newer-session trigger is evaluated at settle time. A conflict on
// an unsettled, still-unjudged placeholder row (next_verdict = ”)
// refreshes verdict and evidence — a backfill can mint a
// placeholder for a concurrent session's in-flight turn, and the
// real prompt's record must repair it. 'unknown' is a judged cue
// verdict, not a placeholder: a judged verdict or a frozen row is
// never rewritten.
func (s *service) RecordTurnOutcome(ctx context.Context, o TurnOutcome) error {
	if o.SessionID == "" || o.TurnID == "" {
		return nil
	}
	paths, _ := json.Marshal(o.EditedPaths)
	hunks, _ := json.Marshal(o.EditHunks)
	added := marshalNoEscape(s.buildHunkAdded(o.HunkAdded, o.EditedPaths))
	cmds, _ := json.Marshal(o.Commands)
	now := s.now().UnixMilli()
	createdAt := o.CreatedAt
	if createdAt <= 0 {
		createdAt = now
	}
	return s.q.InsertTurnOutcome(ctx, db.InsertTurnOutcomeParams{
		SessionID:    o.SessionID,
		TurnID:       o.TurnID,
		ProjectKey:   s.projectKey,
		EditedPaths:  string(paths),
		EditHunks:    string(hunks),
		Commands:     string(cmds),
		HunkAdded:    string(added),
		NextVerdict:  o.NextVerdict,
		SettleAt:     createdAt + settleWindowMillis,
		ParamVersion: s.paramVersion,
		CreatedAt:    createdAt,
	})
}

// ListRecentSessionIDs exposes the session ids a backfill scans —
// sessions demonstrably in this project (the ledger-membership proof
// the query carries), except the live one, newest first. Sessions
// stamp created_at in seconds.
func (s *service) ListRecentSessionIDs(ctx context.Context, excludeID string, limit int) ([]db.ListRecentSessionsRow, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	return s.q.ListRecentSessions(ctx, db.ListRecentSessionsParams{
		ID:           excludeID,
		ProjectKey:   s.projectKey,
		ProjectKey_2: s.projectKey,
		ProjectKey_3: s.projectKey,
		Limit:        int64(limit),
	})
}

// HasTurnOutcome reports whether a (session, turn) row exists — the
// backfill's skip check so already-recorded turns cost no parse.
func (s *service) HasTurnOutcome(ctx context.Context, sessionID, turnID string) (bool, error) {
	n, err := s.q.HasTurnOutcome(ctx, db.HasTurnOutcomeParams{
		SessionID: sessionID,
		TurnID:    turnID,
	})
	return n > 0, err
}

// SettleTurnOutcomes freezes the verdicts of turn rows whose window
// closed — same dual trigger as episode settlement, same frozen-once
// contract. Bounded per call; runs detached off the hot path.
func (s *service) SettleTurnOutcomes(ctx context.Context, limit int) error {
	// Same guard as the episode pass: writes are idempotent, so a
	// skipped pass under contention costs nothing.
	if !s.labelMu.TryLock() {
		return nil
	}
	defer s.labelMu.Unlock()
	if limit <= 0 {
		limit = defaultListLimit
	}
	now := s.now().UnixMilli()
	rows, err := s.q.ListUnsettledTurnOutcomes(ctx, db.ListUnsettledTurnOutcomesParams{
		ProjectKey: s.projectKey,
		SettleAt:   now,
		Limit:      int64(limit),
	})
	if err != nil {
		return err
	}
	// Same batching as the episode pass: one git log + one ledger
	// read over the union window, then per-row filtering.
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
		if err := s.settleTurn(ctx, r, winEnds[i], ev, now); err != nil {
			return err
		}
	}
	return nil
}

// settleTurn computes one row's verdict inside its closed window and
// freezes it. The order encodes the review's rules: a revision cue
// leads — it is scoped to this very turn, so it judges the unit the
// row certifies — and divergence shares the failure arm; a red
// session-end test state fails a turn whose edits survived;
// acceptance requires survival or external-commit evidence — never a
// default. ReferentEpisodeOutcome intentionally inverts the cue-vs-
// artifact order: an episode's claim is path-scoped, so the file's
// own survival/commit evidence outranks a cue that may object to a
// different file in the same turn.
func (s *service) settleTurn(ctx context.Context, r db.ListUnsettledTurnOutcomesRow, winEnd int64, ev commitEvidence, now int64) error {
	var paths []string
	var hunks map[string][]string
	var cmds []string
	// Columns written by this package are JSON-or-empty; a corrupt
	// cell degrades to zero evidence — the honest failure shape.
	_ = json.Unmarshal([]byte(r.EditedPaths), &paths)
	_ = json.Unmarshal([]byte(r.EditHunks), &hunks)
	_ = json.Unmarshal([]byte(r.Commands), &cmds)

	var added map[string]hunkAddedEntry
	// hunk_added columns written by this package are JSON-or-empty;
	// a corrupt cell degrades to no spans — settle marks the path's
	// region unknown rather than guessing at it.
	_ = json.Unmarshal([]byte(r.HunkAdded), &added)
	later := s.laterEditedPaths(ctx, r.SessionID, r.CreatedAt)

	diverged := false
	// decidable marks "at least one fingerprint produced a readable
	// survival verdict" — either way it read. A diverged read is
	// decidable too, so acceptance must also require !diverged:
	// every readable fingerprint survived.
	decidable := false
	after := make(map[string]hunkAfterEntry, len(paths))
	afterTotal := 0
	for _, p := range paths {
		fr := s.readHunkSource(p)
		var spans [][2]int
		if e, ok := added[p]; ok {
			spans = e.Spans
		}
		after[p] = captureHunkCell(fr, p, spans, later[p], &afterTotal)
		switch fr.state {
		case hunkMissing:
			// The file is gone — nothing survived, when there was
			// a fingerprint to survive. An empty fingerprint (a
			// pure deletion, or a pre-fingerprint row) abstains
			// like the old reader: absence is no evidence. An
			// escaping symlink reports out_of_workspace above and
			// abstains like a textual escape, not a deletion.
			if len(hunks[p]) > 0 {
				decidable, diverged = true, true
			}
		case hunkOK:
			if d := fingerprintDiverged(fr.data, hunks[p]); d.Valid {
				decidable = true
				if d.Int64 == 1 {
					diverged = true
				}
			}
		case hunkUnreadable:
			// Exists but refused the read — same "gone" evidence
			// shape a deletion presents.
			if len(hunks[p]) > 0 {
				decidable, diverged = true, true
			}
		}
		// too_large/binary paths abstain — presence-matching a binary
		// is meaningless, and paying a multi-MB read+scan for a
		// fingerprint is not worth the verdict.
	}
	committed := s.hasRepo && len(paths) > 0 &&
		ev.inWindow(paths, r.CreatedAt, winEnd)
	green := s.sessionTestsGreen(ctx, r.SessionID)

	var verdict string
	switch {
	case len(paths) == 0 && len(cmds) == 0:
		// Nothing happened — a read-only or chat turn is not
		// evidence either way.
		verdict = TurnUnknown
	case diverged || r.NextVerdict == ReferentRevised:
		verdict = TurnRevised
	case green.Valid && green.Int64 == 0:
		// Session-grain by design: the ledger has no per-turn
		// last-exit, so a red session end fails every non-empty
		// turn in it — "failed" means the session ended red, not
		// that this turn's work failed (#353).
		verdict = TurnFailed
	case committed || (decidable && !diverged):
		verdict = TurnAccepted
	case len(paths) == 0 && len(cmds) > 0 && green.Valid && green.Int64 == 1:
		// A command-only turn whose session ended green. Edit-only
		// turns can't reach this arm — a pure deletion leaves no
		// fingerprint to read, so decidable stays false and the row
		// can only accept via commit evidence. Asymmetric by design:
		// missing evidence is never acceptance.
		verdict = TurnAccepted
	default:
		verdict = TurnUnknown
	}
	return s.q.SettleTurnOutcome(ctx, db.SettleTurnOutcomeParams{
		Verdict:   sql.NullString{String: verdict, Valid: true},
		SettledAt: sql.NullInt64{Int64: now, Valid: true},
		HunkAfter: marshalNoEscape(after),
		ID:        r.ID,
	})
}

// Hunk-content bounds (#394). Runes, not bytes — text columns are
// line-shaped evidence, and a rune cap keeps the worst case near
// 4× before JSON escaping. Sensitive-path content is never stored.
const (
	maxHunkPathRunes = 16 * 1024 // per path, per column
	maxHunkRowRunes  = 64 * 1024 // per row, per column
	// maxHunkReadBytes gates the settle-time read: a lockfile or
	// generated blob over this abstains rather than paying a
	// whole-file scan for the fingerprint and the capture.
	maxHunkReadBytes = 1 << 20
	// maxHunkLocateBytes gates the record-time span search the same
	// way — beyond it the region stays unknown rather than pinning a
	// big read to the record pass.
	maxHunkLocateBytes = 4 << 20
	// hunkContextLines is the slack kept on each side of a stored
	// span when hunk_after slices it — enough to catch the user's
	// local rewrite without dragging unrelated file in.
	hunkContextLines = 32
	// maxHunkSpans bounds located spans per path; beyond it the set
	// collapses to the min-max envelope.
	maxHunkSpans = 32
)

// sensitiveBasenames are file names whose content never persists in
// the hunk columns: credential-carrying configs and key material.
// Named-key redaction can't catch free-form secret files (URL
// credentials, webhook secrets, connection strings), so these paths
// record a state instead of text — on both sides of the pair.
var sensitiveBasenames = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx",
	"*.keystore", "id_*", "credentials*", "secrets.*",
	".netrc", ".npmrc", ".pypirc",
}

func sensitivePath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	for _, pat := range sensitiveBasenames {
		if ok, _ := filepath.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// hunkAddedEntry is one path's record-time cell in hunk_added: the
// turn's verbatim edits, plus the line spans each edit's new text
// occupied in the file at record time — the anchor that lets
// settlement slice the same region's post-window content instead of
// the whole file. Empty spans means the region could not be located.
type hunkAddedEntry struct {
	Edits     []HunkEdit `json:"edits,omitempty"`
	Spans     [][2]int   `json:"spans,omitempty"`
	State     string     `json:"state,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// hunkAfterEntry is one path's settle-time cell in hunk_after:
// per-span windows of the post-window file, or the state naming why
// no content was captured.
type hunkAfterEntry struct {
	State            string   `json:"state"`
	Windows          []string `json:"windows,omitempty"`
	Truncated        bool     `json:"truncated,omitempty"`
	LaterTurnTouched bool     `json:"later_turn_touched,omitempty"`
}

// Hunk-content states — why an entry carries no text.
const (
	hunkOK             = "ok"
	hunkMissing        = "missing"
	hunkUnreadable     = "unreadable"
	hunkBinary         = "binary"
	hunkTooLarge       = "too_large"
	hunkSensitive      = "sensitive"
	hunkOutOfWorkspace = "out_of_workspace"
	hunkOverCap        = "over_cap"
	// region_unknown: the file read fine but the record pass located
	// no span — the region the agent wrote can't be identified at
	// settle (already rewritten, legacy row, pure deletion).
	hunkRegionUnknown = "region_unknown"
)

// hunkFileRead is one settle-time read of an edited path: the state
// the capture records, and the content when the state carries it.
type hunkFileRead struct {
	data  []byte
	state string
}

// readHunkSource performs the one-per-path settle read: workspace
// and symlink gates, a size stat before the content read so a huge
// file never pays the scan, then binary detection. The same content
// feeds the survival check and the window capture, so the verdict
// and the snapshot can never disagree.
func (s *service) readHunkSource(path string) hunkFileRead {
	if !workspacePathSafe(path) || s.pathEscapesWorkspace(path) {
		return hunkFileRead{state: hunkOutOfWorkspace}
	}
	if _, err := os.Lstat(filepath.Join(s.workingDir, filepath.FromSlash(path))); err != nil {
		return hunkFileRead{state: hunkMissing}
	}
	root, err := os.OpenRoot(s.workingDir)
	if err != nil {
		return hunkFileRead{state: hunkMissing}
	}
	defer root.Close()
	fi, err := root.Stat(filepath.FromSlash(path))
	if err != nil {
		return hunkFileRead{state: hunkUnreadable}
	}
	if fi.Size() > maxHunkReadBytes {
		return hunkFileRead{state: hunkTooLarge}
	}
	data, err := root.ReadFile(filepath.FromSlash(path))
	if err != nil {
		return hunkFileRead{state: hunkUnreadable}
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return hunkFileRead{state: hunkBinary}
	}
	return hunkFileRead{data: data, state: hunkOK}
}

// buildHunkAdded writes each path's record-time cell (#394): the
// verbatim edits — redacted, bounded at whole-edit granularity — and
// the line spans those edits occupy in the file NOW, located before
// the next turn's writes can move them. Sensitive paths store only
// their state. Iteration follows edited_paths order so the row cap
// cuts tail paths deterministically; dropped entries carry over_cap
// rather than vanishing.
func (s *service) buildHunkAdded(raw map[string][]HunkEdit, order []string) map[string]hunkAddedEntry {
	out := make(map[string]hunkAddedEntry, len(raw))
	total := 0
	for _, p := range order {
		e := hunkAddedEntry{}
		switch {
		case sensitivePath(p):
			e.State = hunkSensitive
		case len(raw[p]) == 0:
			// No recorded edits — the path carries no cell rather
			// than an empty one a miner could misread.
			continue
		case total >= maxHunkRowRunes:
			e.State = hunkOverCap
		default:
			e.Edits, e.Truncated = boundHunkEdits(raw[p],
				min(maxHunkPathRunes, maxHunkRowRunes-total))
			if len(e.Edits) == 0 && len(raw[p]) > 0 {
				e.State = hunkOverCap
				break
			}
			for _, ed := range e.Edits {
				total += len([]rune(ed.Old)) + len([]rune(ed.New))
			}
			// Spans locate the ORIGINAL text in the file — the
			// stored copy is redacted, and a [REDACTED] marker can
			// never match. boundHunkEdits returns a prefix, so the
			// kept count indexes raw.
			e.Spans = s.locateHunkSpans(p, raw[p][:len(e.Edits)])
		}
		out[p] = e
	}
	return out
}

// boundHunkEdits keeps edits within the rune budget — redacted
// first so a credential never persists, then counted. A cut mid-list
// flags truncation and stops at the boundary, so every stored
// {old,new} pair stays whole for the miner; a first edit bigger than
// the entire budget keeps its new truncated rather than storing
// nothing. The result is always a prefix of the input — the caller
// indexes raw for span location.
func boundHunkEdits(edits []HunkEdit, budget int) ([]HunkEdit, bool) {
	var kept []HunkEdit
	for _, e := range edits {
		e.Old = redact.Secrets(e.Old)
		e.New = redact.Secrets(e.New)
		cost := len([]rune(e.Old)) + len([]rune(e.New))
		if cost <= budget {
			budget -= cost
			kept = append(kept, e)
			continue
		}
		if len(kept) == 0 {
			// The first edit may not exceed the budget either —
			// split it evenly so the agent's new text keeps at
			// least half while the pair stays within the cap.
			half := max(budget/2, 0)
			e.Old = truncateRunes(e.Old, half)
			e.New = truncateRunes(e.New, budget-len([]rune(e.Old)))
			kept = append(kept, e)
		}
		return kept, true
	}
	return kept, false
}

// locateHunkSpans finds each edit's new text in the file as it
// stands at record time and converts it to line spans — the anchors
// settlement slices around. An edit whose text can't be found (the
// user or a folded-in turn already rewrote it) contributes no span;
// a pure deletion likewise. Spans are merged and bounded, so a
// scattered multiedit can't pin unbounded windows.
func (s *service) locateHunkSpans(path string, edits []HunkEdit) [][2]int {
	if sensitivePath(path) {
		return nil
	}
	if fi, err := os.Lstat(filepath.Join(s.workingDir, filepath.FromSlash(path))); err != nil ||
		fi.Size() > maxHunkLocateBytes {
		return nil
	}
	data, ok := s.readWorkspaceFile(path)
	if !ok {
		return nil
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	var spans [][2]int
	var claimed [][2]int // byte ranges already attributed to an edit
	for _, e := range edits {
		if e.New == "" {
			continue
		}
		// First occurrence not already claimed by an earlier edit:
		// two calls writing identical text must not share one anchor.
		idx := -1
		for from := 0; ; {
			i := strings.Index(content[from:], e.New)
			if i < 0 {
				break
			}
			i += from
			end := i + len(e.New)
			clash := false
			for _, c := range claimed {
				if i < c[1] && end > c[0] {
					clash = true
					break
				}
			}
			if !clash {
				idx = i
				break
			}
			from = end
		}
		if idx < 0 {
			idx = strings.Index(content, e.New)
		}
		if idx < 0 {
			continue
		}
		claimed = append(claimed, [2]int{idx, idx + len(e.New)})
		start := strings.Count(content[:idx], "\n")
		end := start + strings.Count(e.New, "\n") + 1
		spans = append(spans, [2]int{start, end})
	}
	return mergeSpans(spans)
}

// mergeSpans sorts and merges overlapping or touching half-open
// line spans. More than maxHunkSpans fragments collapse into the
// single min–max envelope — still bounded, still honest about which
// file region the turn wrote.
func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) == 0 {
		return nil
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	out := [][2]int{spans[0]}
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s[0] <= last[1] {
			last[1] = max(last[1], s[1])
		} else {
			out = append(out, s)
		}
	}
	if len(out) > maxHunkSpans {
		return [][2]int{{out[0][0], out[len(out)-1][1]}}
	}
	return out
}

// captureHunkCell slices the post-window content of one edited path
// around the record-time spans (#394). The cell records an explicit
// state for every non-content outcome; when the region was never
// located, region_unknown is written rather than a misleading file
// head. Later-turn touch is flagged so the preference pair isn't
// read as a user revision when the next turn rewrote the region.
func captureHunkCell(fr hunkFileRead, path string, spans [][2]int, later bool, rowTotal *int) hunkAfterEntry {
	e := hunkAfterEntry{State: fr.state, LaterTurnTouched: later}
	if fr.state != hunkOK {
		return e
	}
	switch {
	case sensitivePath(path):
		e.State = hunkSensitive
	case *rowTotal >= maxHunkRowRunes:
		// The row is full — over_cap names why nothing was even
		// attempted, ahead of the no-spans case.
		e.State = hunkOverCap
	case len(spans) == 0:
		e.State = hunkRegionUnknown
	default:
		lines := strings.Split(string(fr.data), "\n")
		budget := min(maxHunkPathRunes, maxHunkRowRunes-*rowTotal)
		for _, w := range spanWindows(lines, spans) {
			text := redact.Secrets(w)
			if len([]rune(text)) > budget {
				e.Truncated = true
				if budget <= 0 {
					break
				}
				text = truncateRunes(text, budget)
			}
			e.Windows = append(e.Windows, text)
			used := len([]rune(text))
			budget -= used
			*rowTotal += used
		}
		if len(e.Windows) == 0 && !e.Truncated {
			e.State = hunkRegionUnknown
		}
	}
	return e
}

// spanWindows expands each span by the context slack, clamps to the
// file, merges overlaps, and returns the windowed text chunks.
func spanWindows(lines []string, spans [][2]int) []string {
	var out []string
	for _, s := range mergeSpans(expandSpans(spans, hunkContextLines)) {
		s[0] = max(s[0], 0)
		s[1] = min(s[1], len(lines))
		if s[0] < s[1] {
			out = append(out, strings.Join(lines[s[0]:s[1]], "\n"))
		}
	}
	return out
}

func expandSpans(spans [][2]int, ctx int) [][2]int {
	out := make([][2]int, len(spans))
	for i, s := range spans {
		out[i] = [2]int{s[0] - ctx, s[1] + ctx}
	}
	return out
}

// fingerprintDiverged is the survival check over already-read file
// content — the line-presence half of hunkDiverged lifted so
// settleTurn's single read feeds both verdict and capture.
func fingerprintDiverged(data []byte, fingerprint []string) sql.NullInt64 {
	if len(fingerprint) == 0 {
		return sql.NullInt64{}
	}
	fileLines := map[string]struct{}{}
	for _, l := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			fileLines[t] = struct{}{}
		}
	}
	present := 0
	for _, l := range fingerprint {
		if _, ok := fileLines[l]; ok {
			present++
		}
	}
	if float64(present)/float64(len(fingerprint)) < survivalMajority {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

// laterEditedPaths unions the edited_paths of the turn rows recorded
// after this one in the same session — the flag that separates "user
// revised the region" from "a later agent turn rewrote it" (#400
// review). Best-effort: a later turn whose row hasn't opened yet
// can't mark the flag.
func (s *service) laterEditedPaths(ctx context.Context, sessionID string, createdAt int64) map[string]bool {
	out := map[string]bool{}
	rows, err := s.q.ListLaterTurnEditPaths(ctx, db.ListLaterTurnEditPathsParams{
		SessionID: sessionID,
		CreatedAt: createdAt,
	})
	if err != nil {
		return out
	}
	for _, r := range rows {
		var ps []string
		if json.Unmarshal([]byte(r), &ps) == nil {
			for _, p := range ps {
				out[p] = true
			}
		}
	}
	return out
}

// marshalNoEscape is json.Marshal without HTML escaping — hunk
// content stores code, and <, >, & must read as themselves in
// sqlite3 rather than \u003c.
func marshalNoEscape(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(v) != nil {
		return "{}"
	}
	return strings.TrimRight(b.String(), "\n")
}

// encodeLines re-marshals a hunk's line slice for hunkDiverged's
// JSON-input shape.
func encodeLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	b, _ := json.Marshal(lines)
	return string(b)
}
