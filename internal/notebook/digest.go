package notebook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
)

// errDigestExists aborts the commit transaction when the in-tx
// re-check finds a sibling generation's digest for the same turn —
// an outcome, not a failure.
var errDigestExists = errors.New("turn digest already exists")

// digestTitleRe matches a model-emitted "Turn {N} digest" headline
// prefix — number optional — so GenerateTurnDigest can restamp the
// structural part while keeping the model's topic.
var digestTitleRe = regexp.MustCompile(`(?i)^\s*turn\s+\d*\s+digest\b`)

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
	events := classifyAll(req.Msgs)
	if len(events) == 0 {
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

	// The turn's decision demotes behind its digest like every other
	// same-turn entry, so the decision itself must be digest input —
	// mirror GenerateEntries' fold of assistant-text decisions. It
	// joins the significant budget class, ordered last.
	if hasDecision(req.Msgs) {
		events = append(events, EntryInput{
			EventType:   EventDecision,
			Title:       "Decision",
			Description: truncate(extractAssistantText(req.Msgs), 2000),
			Succeeded:   true,
		})
	}
	interrupted := turnInterrupted(req.Msgs)
	entry, err := s.generator.GenerateDigest(ctx, sessionID, buildDigestInput(events, req.TurnNumber, interrupted, digestUserPrompt(req.Msgs)))
	if err != nil {
		return false, fmt.Errorf("failed to generate turn digest: %w", err)
	}
	entry.EventType = EventCheckpoint
	// The headline's turn number is structural — stamped like the
	// tags rather than trusted to the model. A model-emitted "Turn N
	// digest" prefix is restamped with the real number (the topic
	// survives); any other title gains the prefix so a wrong or
	// missing N can never reach the visible headline.
	if loc := digestTitleRe.FindStringIndex(entry.Title); loc != nil {
		entry.Title = fmt.Sprintf("Turn %d digest%s", req.TurnNumber, entry.Title[loc[1]:])
	} else if topic := strings.TrimSpace(entry.Title); topic == "" || topic == "Entry" {
		entry.Title = fmt.Sprintf("Turn %d digest", req.TurnNumber)
	} else {
		entry.Title = fmt.Sprintf("Turn %d digest — %s", req.TurnNumber, topic)
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

// HasFinishedToolCall reports whether msgs contain a finished tool
// call — the digest floor. classifyAll emits one event per finished
// call, so this exactly predicts whether a turn can produce a digest
// without running the full classification.
func HasFinishedToolCall(msgs []message.Message) bool {
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls() {
			if tc.Finished {
				return true
			}
		}
	}
	return false
}

// digestUserPrompt returns the turn's opening user line, truncated —
// one line of intent in the digest input so a turn whose tool calls
// don't self-describe their purpose still digests with context.
func digestUserPrompt(msgs []message.Message) string {
	for _, m := range msgs {
		if m.Role == message.User {
			return truncate(m.Content().Text, 300)
		}
	}
	return ""
}

// turnInterrupted reports whether the turn's last finish-marked
// assistant message ended on FinishReasonCanceled —
// persistCanceledTurn's marker for a user abort — or
// FinishReasonError, a provider failure mid-turn. Either way the
// turn's events are partial, so the digest headline notes
// "interrupted" rather than reading as finished work.
func turnInterrupted(msgs []message.Message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Assistant {
			continue
		}
		if f := msgs[i].FinishPart(); f != nil {
			return f.Reason == message.FinishReasonCanceled ||
				f.Reason == message.FinishReasonError
		}
	}
	return false
}

// buildDigestInput renders the turn-digest input: the finished turn's
// classified events — significant and trivial alike — emitted in
// chronological order, preceded by the turn's opening user line for
// intent context. Significant events are the digest's substance and
// claim the byte budget first, newest first; trivial exploration
// fills what remains. Interrupted turns get a preamble so the model
// marks the headline.
func buildDigestInput(events []EntryInput, turn int64, interrupted bool, userPrompt string) string {
	blocks := make([]string, len(events))
	for i, ev := range events {
		var b strings.Builder
		fmt.Fprintf(&b, "### %s — %s\n%s\n", ev.EventType, ev.Title, ev.Description)
		if ev.ErrorHeadline != "" {
			fmt.Fprintf(&b, "Error headline: %s\n", ev.ErrorHeadline)
		}
		if ev.Verified != "" {
			fmt.Fprintf(&b, "Verification: %s\n", ev.Verified)
		}
		b.WriteString("\n")
		blocks[i] = b.String()
	}

	selected := make([]bool, len(events))
	used := 0
	truncated := false
	for _, wantTrivial := range []bool{false, true} {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].trivial != wantTrivial {
				continue
			}
			if used+len(blocks[i]) > checkpointTailMaxBytes {
				truncated = true
				continue // Skip the oversized event; smaller ones may fit.
			}
			used += len(blocks[i])
			selected[i] = true
		}
	}

	var sb strings.Builder
	if interrupted {
		sb.WriteString("This turn was interrupted before it finished — its events are partial.\n\n")
	}
	if userPrompt != "" {
		fmt.Fprintf(&sb, "Turn %d began with the user asking: %s\n\n", turn, userPrompt)
	}
	fmt.Fprintf(&sb, "Classified events from turn %d (oldest first):\n\n", turn)
	if truncated {
		sb.WriteString("(events elided for budget)\n\n")
	}
	for i := range events {
		if selected[i] {
			sb.WriteString(blocks[i])
		}
	}
	return sb.String()
}
