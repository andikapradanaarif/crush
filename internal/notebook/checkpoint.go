package notebook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/toolclass"
)

// checkpointInputMaxBytes bounds the committed-entries portion of the
// consolidation input. Newest entries win the budget: the checkpoint
// restates the current position, and the oldest history is the most
// likely to already be consolidated into an earlier finer-grain entry.
const checkpointInputMaxBytes = 48_000

// checkpointTailMaxBytes bounds the uncovered-tail event descriptions.
// The tail is the newest evidence so it wins newest-first within its
// own budget — a long uncovered tail (segment generation failing, or
// the first run after enabling the notebook on a big session) must not
// produce an unbounded small-model prompt.
const checkpointTailMaxBytes = 24_000

// GenerateCheckpoint implements the Service interface.
//
// The checkpoint consolidates "what is established, with evidence"
// versus "what is still open" into one entry the model can consult
// instead of re-deriving the investigation from raw history. Input is
// cumulative: every committed entry in the session feeds it — the
// checkpoint restates the whole position, so facts established before
// the previous checkpoint must remain reachable (finer-grain
// checkpoints feed it, same-or-coarser ones never do — the checkpoint
// replaces them rather than stacking on them) — plus classified
// events for the uncovered tail. The newest same-or-coarser
// checkpoint is only a floor marker: entries at or below it do not
// count toward the gathered threshold.
//
// Two checks run before the small-model call: the run tag dedup (a
// checkpoint already written under req.RunTag) and the gathered-count
// floor (req.MinExploration, in classified non-trivial non-mutating
// event units — consciously different units from the scope gate's
// raw call count). Two imprecisions are accepted here: a run of only
// trivial calls (grep, glob, small views) plus writes tallies zero —
// it self-heals when the next run sees the committed segment entries
// — and a segment that commits between tail computation and this
// read appears both as an entry and as raw tail events, counted
// twice (bounded to one segment's worth). The entry commits inside
// the same write
// transaction that allocates its event number, matching segment
// generation's collision discipline; a tag re-check inside the
// transaction closes the claim/land gap between concurrent
// generations.
func (s *service) GenerateCheckpoint(ctx context.Context, sessionID string, req CheckpointRequest) (bool, error) {
	entries, err := s.GetEntries(ctx, sessionID)
	if err != nil {
		return false, err
	}

	// The cutoff is the newest same-or-coarser checkpoint: entries at
	// or below it are already consolidated. gathered tallies
	// everything newer — that is what "context gathered since the
	// last checkpoint" means, and it doubles as the check that keeps
	// a no-new-work run from rewriting an identical position.
	cutoffRank := granularityRank(req.Granularity)
	var cutoffTurn, cutoffEvent int64
	haveCutoff := false
	var inputEntries []Entry
	for _, e := range entries {
		if e.EventType == EventCheckpoint {
			// The run tag dedups checkpoints only — a model that
			// happens to emit #run:<n> on an ordinary entry must not
			// suppress this run's checkpoint.
			if req.RunTag != "" && slices.Contains(e.Tags, req.RunTag) {
				return false, nil
			}
			g := CheckpointGranularity(e)
			if granularityRank(g) >= cutoffRank {
				if !haveCutoff || e.TurnNumber > cutoffTurn ||
					(e.TurnNumber == cutoffTurn && e.EventNumber > cutoffEvent) {
					haveCutoff = true
					cutoffTurn, cutoffEvent = e.TurnNumber, e.EventNumber
				}
				continue // Same-or-coarser checkpoints never feed a checkpoint.
			}
		}
		inputEntries = append(inputEntries, e)
	}

	gathered := 0
	for _, e := range inputEntries {
		if e.TurnNumber > cutoffTurn || (e.TurnNumber == cutoffTurn && e.EventNumber > cutoffEvent) {
			gathered++
		}
	}

	significant, _ := classifyEvents(req.Msgs)
	var tailInputs []EntryInput
	for _, ev := range significant {
		// Mutating events join the input — what changed is part of
		// the position — but only non-mutating exploration counts
		// toward the floor: the floor measures investigation depth,
		// not write volume.
		if ev.ToolCall == nil || !toolclass.IsMutatingCall(ev.ToolCall.Name, ev.ToolCall.Input) {
			gathered++
		}
		tailInputs = append(tailInputs, ev)
	}
	if gathered < req.MinExploration {
		return false, nil
	}

	input := buildCheckpointInput(inputEntries, tailInputs, cutoffTurn, cutoffEvent)
	entry, err := s.generator.GenerateCheckpoint(ctx, sessionID, input)
	if err != nil {
		return false, fmt.Errorf("failed to generate checkpoint: %w", err)
	}
	entry.EventType = EventCheckpoint
	if entry.Title == "" || entry.Title == "Entry" {
		entry.Title = "Checkpoint"
	}
	// Structural tags are stamped, not generated: granularity, run
	// dedup, and phase must reflect the request, not the model's
	// output. Strip model-emitted tags in those namespaces first — a
	// hallucinated #granularity:session would misrank the entry (the
	// first granularity: tag wins) and a spurious #run:N could falsely
	// dedup a future run's checkpoint.
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
	if req.Granularity != "" {
		stampTag(granularityTagPrefix + req.Granularity)
	}
	if req.RunTag != "" {
		stampTag(req.RunTag)
	}
	if estimateTokens(entry.Text) > s.opts.MaxEntryTokens {
		entry.Text = truncateEntry(entry.Text, s.opts.MaxEntryTokens)
	}

	err = s.withTx(ctx, func(q *db.Queries) error {
		if req.RunTag != "" {
			// Re-check under the write lock: a sibling generation
			// that claimed first may have committed while this one
			// was in the model call.
			dup, err := q.SearchNotebookByTag(ctx, db.SearchNotebookByTagParams{
				SessionID: sessionID,
				Tag:       req.RunTag,
			})
			if err != nil {
				return err
			}
			for _, row := range dup {
				if row.EventType == EventCheckpoint {
					return errCheckpointExists
				}
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
	if errors.Is(err, errCheckpointExists) {
		return false, nil
	}
	if err != nil && req.RunTag != "" {
		// withTx opens a deferred SQLite transaction: a sibling that
		// commits between the in-tx re-check and this write fails the
		// commit with SQLITE_BUSY_SNAPSHOT — a lost dedup race, not a
		// real error. If the sibling's checkpoint is now visible,
		// report the clean dedup outcome.
		existing, serr := s.SearchByTag(ctx, sessionID, req.RunTag)
		if serr == nil {
			for _, e := range existing {
				if e.EventType == EventCheckpoint {
					return false, nil
				}
			}
		}
	}
	if err != nil {
		return false, fmt.Errorf("failed to commit checkpoint: %w", err)
	}

	if err := s.Compact(ctx, sessionID); err != nil {
		slog.Error("Failed to compact notebook", "error", err)
	}
	return true, nil
}

// errCheckpointExists aborts the commit transaction when the run tag
// re-check inside withTx finds a sibling's checkpoint — it is an
// outcome, not a failure.
var errCheckpointExists = errors.New("checkpoint already exists for run")

// buildCheckpointInput renders the consolidation input: committed
// entries in chronological order, newest filling the byte budget
// first, then the raw descriptions of the uncovered tail's
// significant events within their own newest-first budget. The
// (cutoffTurn, cutoffEvent) divider marks where the previous
// same-or-coarser checkpoint's coverage ended, so the model can tell
// already-consolidated history from newly gathered work.
func buildCheckpointInput(entries []Entry, tail []EntryInput, cutoffTurn, cutoffEvent int64) string {
	// Walk newest-first to spend the budget on the freshest state,
	// then reverse into reading order. Entries at or below the cutoff
	// are already consolidated into the previous checkpoint; the rest
	// are new since it.
	var blocks, freshBlocks []string
	used := 0
	elided := false
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var b strings.Builder
		fmt.Fprintf(&b, "## Turn %d.%d — %s (%s)\n", e.TurnNumber, e.EventNumber, e.Title, e.EventType)
		text := e.EntryTextFull
		if text == "" {
			text = e.EntryText
		}
		b.WriteString(text)
		b.WriteString("\n\n")
		if used+b.Len() > checkpointInputMaxBytes {
			elided = true
			continue
		}
		used += b.Len()
		if e.TurnNumber > cutoffTurn || (e.TurnNumber == cutoffTurn && e.EventNumber > cutoffEvent) {
			freshBlocks = append(freshBlocks, b.String())
		} else {
			blocks = append(blocks, b.String())
		}
	}
	slices.Reverse(blocks)
	slices.Reverse(freshBlocks)

	// The tail gets its own newest-first budget — it is the newest
	// evidence and the reason the checkpoint exists, but a long
	// uncovered tail must not overflow the small-model prompt.
	var tailBlocks []string
	tailUsed := 0
	tailTruncated := false
	for i := len(tail) - 1; i >= 0; i-- {
		ev := tail[i]
		b := fmt.Sprintf("### %s — %s\n%s\n\n", ev.EventType, ev.Title, ev.Description)
		if tailUsed+len(b) > checkpointTailMaxBytes {
			tailTruncated = true
			continue // Skip the oversized event; smaller older ones may fit.
		}
		tailUsed += len(b)
		tailBlocks = append(tailBlocks, b)
	}
	slices.Reverse(tailBlocks)

	var sb strings.Builder
	sb.WriteString("Committed notebook entries (oldest first):\n\n")
	if elided {
		// Without the marker the model cannot tell "no prior
		// history" from "history crowded out by the budget".
		sb.WriteString("(older entries elided)\n\n")
	}
	if len(blocks) == 0 && len(freshBlocks) == 0 && !elided {
		sb.WriteString("(none)\n\n")
	}
	for _, b := range blocks {
		sb.WriteString(b)
	}
	if cutoffTurn > 0 && len(freshBlocks) > 0 {
		fmt.Fprintf(&sb, "— gathered since the last checkpoint (turn %d, event %d) —\n\n", cutoffTurn, cutoffEvent)
	}
	for _, b := range freshBlocks {
		sb.WriteString(b)
	}
	sb.WriteString("Recent uncovered events (oldest first):\n\n")
	if tailTruncated {
		sb.WriteString("(older tail events elided)\n\n")
	}
	for _, b := range tailBlocks {
		sb.WriteString(b)
	}
	return sb.String()
}
