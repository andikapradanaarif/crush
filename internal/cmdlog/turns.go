// Turn-level outcome labels (#340): the acceptance verdict at the
// unit procedural memory mines — one row per (session, user turn),
// carrying what the turn did (edited paths, added-line fingerprints,
// commands) and what the follow-up judged (next_verdict). Rows open
// unsettled and freeze once at their observation window's close —
// the same settlement contract the referent labels follow, so the
// fold and the calibrator never read a label that can still change.
package cmdlog

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/charmbracelet/crush/internal/db"
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
// an unsettled, still-unjudged row (next_verdict ” or 'unknown')
// refreshes verdict and evidence — the backfill can mint a
// placeholder for a concurrent session's in-flight turn, and the
// real prompt's record must repair it. A judged verdict or a frozen
// row is never rewritten.
func (s *service) RecordTurnOutcome(ctx context.Context, o TurnOutcome) error {
	if o.SessionID == "" || o.TurnID == "" {
		return nil
	}
	paths, _ := json.Marshal(o.EditedPaths)
	hunks, _ := json.Marshal(o.EditHunks)
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

	diverged := false
	survivable := false
	for _, p := range paths {
		d := s.hunkDiverged(p, encodeLines(hunks[p]))
		if !d.Valid {
			continue
		}
		survivable = true
		if d.Int64 == 1 {
			diverged = true
		}
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
		verdict = TurnFailed
	case committed || (survivable && !diverged):
		verdict = TurnAccepted
	case len(paths) == 0 && len(cmds) > 0 && green.Valid && green.Int64 == 1:
		// A command-only turn whose session ended green.
		verdict = TurnAccepted
	default:
		verdict = TurnUnknown
	}
	return s.q.SettleTurnOutcome(ctx, db.SettleTurnOutcomeParams{
		Verdict:   sql.NullString{String: verdict, Valid: true},
		SettledAt: sql.NullInt64{Int64: now, Valid: true},
		ID:        r.ID,
	})
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
