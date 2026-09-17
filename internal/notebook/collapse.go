package notebook

import (
	"context"
	"fmt"

	"github.com/charmbracelet/crush/internal/db"
)

// CounterPriorTurnResultRecall names the session counter for result:
// recalls that resolve to a tool call in a prior turn — the feasible
// approximation of "recall into a collapsed turn", since collapse
// leaves no stored mark for recallToolResult to read.
const CounterPriorTurnResultRecall = "prior_turn_result_recalls"

// RecordCollapsedTurn persists that a prior turn rendered collapsed:
// the flag-flip evidence for the stub/digest modes. Idempotent per
// (session, turn) — renders re-collapse the same turn every step and
// resumed sessions collapse it across processes, so the row's
// existence is the dedupe. Reports whether the row was new so callers
// accumulate each turn once; events is the collapsed call/result pair
// count, fixed once a turn completes.
func (s *service) RecordCollapsedTurn(ctx context.Context, sessionID string, turnNumber int64, events int) (bool, error) {
	affected, err := s.q.RecordCollapsedTurn(ctx, db.RecordCollapsedTurnParams{
		SessionID:  sessionID,
		TurnNumber: turnNumber,
		Events:     int64(events),
	})
	if err != nil {
		return false, fmt.Errorf("failed to record collapsed turn: %w", err)
	}
	return affected > 0, nil
}

// BumpSessionCounter adds delta to a named per-session counter —
// scalar telemetry that doesn't fit the per-turn grain of
// collapsed_turns (e.g. CounterPriorTurnResultRecall).
func (s *service) BumpSessionCounter(ctx context.Context, sessionID, name string, delta int64) error {
	return s.q.BumpSessionCounter(ctx, db.BumpSessionCounterParams{
		SessionID: sessionID,
		Name:      name,
		Value:     delta,
	})
}
