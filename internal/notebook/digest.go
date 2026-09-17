package notebook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
)

// errDigestExists aborts the commit transaction when the in-tx
// re-check finds a sibling generation's digest for the same turn —
// an outcome, not a failure.
var errDigestExists = errors.New("turn digest already exists")

// GenerateTurnDigest implements the Service interface.
//
// The digest consolidates one finished turn's work — what it
// established, which files it touched, what it left open — into a
// checkpoint-shaped entry at granularity:turn. Input is the turn's
// own classified events, both buckets: significant events are the
// substance and trivial events are still the turn's evidence. The
// floor is "any classified tool event" — it exists to skip empty
// turns (pure conversation produces no digest), not to measure
// exploration depth.
//
// Dedup keys on (turn, granularity:turn), checked before the model
// call and re-checked inside the commit transaction like
// errCheckpointExists, so concurrent or retried generations cannot
// write two digests for one turn. There is deliberately no run tag:
// the turn itself is the identity, and a run-scoped tag would let a
// mid-run boundary checkpoint's run suppress this run's digests.
func (s *service) GenerateTurnDigest(ctx context.Context, sessionID string, req DigestRequest) (bool, error) {
	significant, trivial := classifyEvents(req.Msgs)
	if len(significant)+len(trivial) == 0 {
		return false, nil
	}
	// Cheap dedup outside the write lock: an existing digest makes
	// this a no-op before the small-model call.
	existing, err := s.GetByTurn(ctx, sessionID, req.TurnNumber)
	if err != nil {
		return false, err
	}
	for _, e := range existing {
		if CheckpointGranularity(e) == GranularityTurn {
			return false, nil
		}
	}

	interrupted := turnInterrupted(req.Msgs)
	events := make([]EntryInput, 0, len(significant)+len(trivial))
	events = append(events, significant...)
	events = append(events, trivial...)
	entry, err := s.generator.GenerateDigest(ctx, sessionID, buildDigestInput(events, interrupted))
	if err != nil {
		return false, fmt.Errorf("failed to generate turn digest: %w", err)
	}
	entry.EventType = EventCheckpoint
	if entry.Title == "" || entry.Title == "Entry" {
		entry.Title = fmt.Sprintf("Turn %d digest", req.TurnNumber)
	}
	// An interrupted turn must not read as finished work — the
	// headline carries the marker whether or not the model wrote it.
	if interrupted && !strings.Contains(strings.ToLower(entry.Title), "interrupt") {
		entry.Title += " — interrupted"
	}
	// Structural tags are stamped, not generated — same discipline as
	// GenerateCheckpoint: a hallucinated #granularity:session would
	// misrank the entry and a spurious #run:N could falsely dedup a
	// boundary checkpoint. Turn digests carry no run tag; the turn is
	// the dedup key.
	entry.Tags = slices.DeleteFunc(entry.Tags, func(t string) bool {
		return strings.HasPrefix(t, granularityTagPrefix) ||
			strings.HasPrefix(t, "run:") || strings.HasPrefix(t, "phase:")
	})
	stampTag := func(tag string) {
		if !slices.Contains(entry.Tags, tag) {
			entry.Tags = append(entry.Tags, tag)
		}
	}
	stampTag("phase:checkpoint")
	stampTag(granularityTagPrefix + GranularityTurn)
	if estimateTokens(entry.Text) > s.opts.MaxEntryTokens {
		entry.Text = truncateEntry(entry.Text, s.opts.MaxEntryTokens)
	}

	err = s.withTx(ctx, func(q *db.Queries) error {
		// Re-check under the write lock: a sibling generation for
		// this turn may have committed while this one was in the
		// model call.
		rows, err := q.GetNotebookEntriesByTurn(ctx, db.GetNotebookEntriesByTurnParams{
			SessionID:  sessionID,
			TurnNumber: req.TurnNumber,
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.EventType != EventCheckpoint {
				continue
			}
			tags, err := q.GetNotebookTagsByEntry(ctx, row.ID)
			if err != nil {
				return err
			}
			if slices.Contains(tags, granularityTagPrefix+GranularityTurn) {
				return errDigestExists
			}
		}
		maxEvent, err := q.GetMaxNotebookEventNumber(ctx, db.GetMaxNotebookEventNumberParams{
			SessionID:  sessionID,
			TurnNumber: req.TurnNumber,
		})
		if err != nil {
			return err
		}
		return storeEntry(ctx, q, sessionID, req.TurnNumber, req.SegmentNumber, maxEvent+1, entry, true, "", "")
	})
	if errors.Is(err, errDigestExists) {
		return false, nil
	}
	if err != nil {
		// withTx opens a deferred SQLite transaction: a sibling that
		// commits between the in-tx re-check and this write fails the
		// commit with SQLITE_BUSY_SNAPSHOT — a lost dedup race, not a
		// real error. If the sibling's digest is now visible, report
		// the clean dedup outcome.
		existing, serr := s.GetByTurn(ctx, sessionID, req.TurnNumber)
		if serr != nil {
			slog.Debug("Turn digest dedup re-check failed", "session_id", sessionID, "error", serr)
		}
		for _, e := range existing {
			if CheckpointGranularity(e) == GranularityTurn {
				slog.Warn("Turn digest commit failed after a sibling digest landed; reporting dedup",
					"session_id", sessionID, "turn", req.TurnNumber, "error", err)
				return false, nil
			}
		}
		return false, fmt.Errorf("failed to commit turn digest: %w", err)
	}

	if err := s.Compact(ctx, sessionID); err != nil {
		slog.Error("Failed to compact notebook", "error", err)
	}
	return true, nil
}

// turnInterrupted reports whether the turn's last assistant message
// finished on FinishReasonCanceled — persistCanceledTurn's marker for
// a turn the user aborted. Its events are partial, so the digest
// headline notes "interrupted" rather than reading as finished work.
func turnInterrupted(msgs []message.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Assistant {
			continue
		}
		if f := msgs[i].FinishPart(); f != nil {
			return f.Reason == message.FinishReasonCanceled
		}
	}
	return false
}

// buildDigestInput renders the turn-digest input: the finished turn's
// classified events — significant and trivial alike — in
// chronological order, newest filling the byte budget first.
// Interrupted turns get a preamble so the model marks the headline.
func buildDigestInput(events []EntryInput, interrupted bool) string {
	var blocks []string
	used := 0
	truncated := false
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		var b strings.Builder
		fmt.Fprintf(&b, "### %s — %s\n%s\n", ev.EventType, ev.Title, ev.Description)
		if ev.ErrorHeadline != "" {
			fmt.Fprintf(&b, "Error headline: %s\n", ev.ErrorHeadline)
		}
		if ev.Verified != "" {
			fmt.Fprintf(&b, "Verification: %s\n", ev.Verified)
		}
		b.WriteString("\n")
		if used+b.Len() > checkpointTailMaxBytes {
			truncated = true
			continue // Skip the oversized event; smaller older ones may fit.
		}
		used += b.Len()
		blocks = append(blocks, b.String())
	}
	slices.Reverse(blocks)

	var sb strings.Builder
	if interrupted {
		sb.WriteString("This turn was interrupted before it finished — its events are partial.\n\n")
	}
	sb.WriteString("Classified tool events from one finished turn (oldest first):\n\n")
	if truncated {
		sb.WriteString("(events elided for budget)\n\n")
	}
	for _, b := range blocks {
		sb.WriteString(b)
	}
	return sb.String()
}
