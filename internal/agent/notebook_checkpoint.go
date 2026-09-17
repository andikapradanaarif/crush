package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
)

// checkpointRunTag is the dedup tag a run's checkpoint carries — the
// durable form of the per-run claim, checked inside the commit
// transaction and by the run-end pass.
func checkpointRunTag(stamp uint64) string {
	return fmt.Sprintf("run:%d", stamp)
}

// runStartIndex returns the index just past the last user message —
// this run's calls start there, so a previous run's writes can never
// trip this run's boundary pre-scan. Caveat: drainQueueForStep folds
// no-RunID prompts into the SAME run — the stamp survives a fold —
// so post-fold the scan bounds to the folded turn and a write
// boundary crossed just before the fold becomes invisible to mid-run
// detection. The run-end fallback still catches it, so the fold edge
// degrades to run-end consolidation rather than losing the checkpoint.
func runStartIndex(msgs []message.Message) int {
	start := 0
	for i, m := range msgs {
		if m.Role == message.User {
			start = i + 1
		}
	}
	return start
}

// checkpointRetryBudget caps mid-run generation attempts per run —
// firstMutatingResult stays true for the rest of the run, so without
// a cap a persistent generator failure would serialize a retry on
// every step.
const checkpointRetryBudget = 2

// claimCheckpoint takes the run's checkpoint slot for the mid-run
// boundary trigger — the inflight claim that dedups detection passes
// while generation is async. False when the stamp already claimed, a
// generation is still in flight, or the run's failure budget is spent.
func (t *segmentTracker) claimCheckpoint(stamp uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	failures := 0
	if t.checkpointFailureRun == stamp {
		failures = t.checkpointFailures
	}
	if t.checkpointStamp == stamp || t.checkpointInFlight ||
		failures >= checkpointRetryBudget {
		return false
	}
	t.checkpointStamp = stamp
	t.checkpointInFlight = true
	return true
}

// checkpointSettled reports whether this run's checkpoint slot is
// already resolved for detection purposes — claimed for this stamp, a
// generation in flight, or the per-run retry budget spent. It mirrors
// claimCheckpoint's rejection conditions without claiming, so the
// per-step trigger can skip the mutating-result pre-scan once the
// run's checkpoint is settled.
func (t *segmentTracker) checkpointSettled(stamp uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	failures := 0
	if t.checkpointFailureRun == stamp {
		failures = t.checkpointFailures
	}
	return t.checkpointStamp == stamp || t.checkpointInFlight ||
		failures >= checkpointRetryBudget
}

// retryCheckpoint re-claims the slot for the run-end pass. A mid-run
// claim that completed without committing (below the boundary
// threshold) must not block the run-end floor — but a genuinely
// in-flight generation does.
func (t *segmentTracker) retryCheckpoint(stamp uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.checkpointInFlight {
		return false
	}
	t.checkpointStamp = stamp
	t.checkpointInFlight = true
	return true
}

// finishCheckpoint resolves the claim. A committed checkpoint or a
// clean not-due outcome keeps the slot claimed for the rest of the
// run — the boundary was evaluated once. A failure releases it so the
// run-end pass can retry, and counts against the per-run retry budget
// so a persistent generator failure cannot loop every step.
func (t *segmentTracker) finishCheckpoint(stamp uint64, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.checkpointInFlight = false
	if failed {
		t.checkpointStamp = 0
		// The failure budget is per-run: the counter belongs to the
		// stamp that accrued it, so a later run starts fresh.
		if t.checkpointFailureRun != stamp {
			t.checkpointFailures = 0
		}
		t.checkpointFailures++
		t.checkpointFailureRun = stamp
	}
}

// firstMutatingResult is the cheap pre-scan: reports whether msgs
// contains a finished write-class call whose result landed without
// error — the investigation→execution boundary's trigger condition.
// It runs on every step before the expensive path (GetEntries +
// classifyEvents inside GenerateCheckpoint) is reached.
func firstMutatingResult(msgs []message.Message) bool {
	succeeded := make(map[string]bool)
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			if !tr.IsError {
				succeeded[tr.ToolCallID] = true
			}
		}
	}
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls() {
			if tc.Finished && tools.IsMutatingCall(tc.Name, tc.Input) && succeeded[tc.ID] {
				return true
			}
		}
	}
	return false
}

// checkpointSegmentKey picks the coverage key for a new checkpoint:
// the session's last closed segment, possibly from an earlier turn.
// An open-segment key can never render while the run lives (the
// boundary must pass the key first), so the closed segment is
// preferred; with no closed segment the open tail's key still serves
// the next run, whose boundary will pass it once the tail's own
// coverage commits.
func checkpointSegmentKey(segs []segment) (segmentKey, bool) {
	for i := len(segs) - 1; i >= 0; i-- {
		if !segs[i].open {
			return segs[i].key(), true
		}
	}
	if len(segs) > 0 {
		return segs[len(segs)-1].key(), true
	}
	return segmentKey{}, false
}

// uncoveredTail returns the messages past the last contiguous run of
// covered segments — the raw slice whose classified events join the
// checkpoint's input. First-gap semantics: a closed-but-unprocessed
// segment (a generation failure) opens the tail at its start, so its
// events are never dropped from the input even when a later segment
// already committed.
func uncoveredTail(msgs []message.Message, segs []segment, processed map[segmentKey]bool) []message.Message {
	tailStart := 0
	for _, s := range segs {
		if s.open {
			continue
		}
		if !processed[s.key()] {
			tailStart = s.start
			break
		}
		tailStart = s.end
	}
	if tailStart > len(msgs) {
		tailStart = len(msgs)
	}
	return msgs[tailStart:]
}

// maybeCheckpointBoundary is the mid-run trigger, called from
// detectSegments so it runs on every per-step rebuild regardless of
// whether the scope gate wraps the toolset. The pre-scan keeps it
// cheap: a run that never lands a successful write never pays for the
// service call. Detection is keyed to the run stamp, which only exists
// under PrepareStep's callContext — the Run-start and summarize
// preparePrompt paths carry no stamp and correctly skip.
func (a *sessionAgent) maybeCheckpointBoundary(ctx context.Context, sessionID string, msgs []message.Message, segs []segment, processed map[segmentKey]bool) {
	if !a.notebookCheckpoint || a.notebook == nil {
		return
	}
	stamp := tools.GetRunStampFromContext(ctx)
	if stamp == 0 || sessionID == "" {
		return
	}
	tracker := a.segmentTracker(sessionID)
	// Settled peek before the O(run-messages) pre-scan: once the run's
	// checkpoint is resolved, later steps skip the scan entirely.
	if tracker.checkpointSettled(stamp) {
		return
	}
	if !firstMutatingResult(msgs[runStartIndex(msgs):]) {
		return
	}
	if !tracker.claimCheckpoint(stamp) {
		return
	}
	key, ok := checkpointSegmentKey(segs)
	if !ok {
		// No key exists — not a generation failure, so the retry
		// budget is untouched; the slot stays claimed since a
		// segment-less run can never produce a key.
		tracker.finishCheckpoint(stamp, false)
		return
	}
	a.spawnCheckpoint(ctx, sessionID, tracker, stamp, notebook.CheckpointRequest{
		TurnNumber:     key.turn,
		SegmentNumber:  key.segment,
		Granularity:    notebook.GranularityBoundary,
		RunTag:         checkpointRunTag(stamp),
		MinExploration: scopeGateMinExploration,
		Msgs:           uncoveredTail(msgs, segs, processed),
	})
}

// generateRunEndCheckpoint is the post-run fallback: a run that
// gathered context but never crossed the write boundary — or whose
// mid-run checkpoint failed or fell under the boundary threshold —
// consolidates at run end. The run-tag existence check doubles as the
// durable dedup when the in-memory claim was lost to a rebuild.
// preTurnMsgCount guards the no-op run — a run that appended no
// messages has nothing new to consolidate under its stamp, matching
// generateRunEndSegments. lastAssistantID bounds the input to this
// run — a user message created by a later run (visible past this
// run's final assistant message) means the tail belongs to that run,
// the same ownership rule generateRunEndSegments applies.
func (a *sessionAgent) generateRunEndCheckpoint(ctx context.Context, sessionID string, msgs []message.Message, preTurnMsgCount int, stamp uint64, registry map[segmentKey]notebook.ProcessedSegment, lastAssistantID string) {
	if !a.notebookCheckpoint || a.notebook == nil || stamp == 0 || sessionID == "" ||
		preTurnMsgCount >= len(msgs) {
		return
	}
	if lastAssistantID != "" {
		lastAsst := -1
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].ID == lastAssistantID {
				lastAsst = i
				break
			}
		}
		// lastAsst < 0 means the run's final assistant message isn't
		// in this list — keep msgs whole rather than cutting at the
		// first user message in history.
		if lastAsst >= 0 {
			for i := lastAsst + 1; i < len(msgs); i++ {
				if msgs[i].Role == message.User {
					msgs = msgs[:i]
					break
				}
			}
		}
	}
	runTag := checkpointRunTag(stamp)
	existing, err := a.notebook.SearchByTag(ctx, sessionID, runTag)
	if err != nil {
		slog.Warn("Failed to check run checkpoint tag", "session_id", sessionID, "error", err)
		return
	}
	for _, e := range existing {
		if e.EventType == notebook.EventCheckpoint {
			return
		}
	}
	tracker := a.segmentTracker(sessionID)
	// A mid-run generation still in flight does not block the run-end
	// pass — if it fails the run still gets its checkpoint, and if it
	// commits the in-transaction run-tag re-check short-circuits this
	// one before the write. The claim is cost control, not
	// correctness, and its return is deliberately discarded: when a
	// mid-run generation holds the slot this spawn runs unclaimed,
	// and its finishCheckpoint can clear the mid-run claim's
	// in-flight flag early — the run-tag dedup makes that overlap
	// cost a redundant model call at most.
	tracker.retryCheckpoint(stamp)
	segs := segmentBoundaries(msgs, a.segTokenBudget(), a.segMaxSteps())
	key, ok := checkpointSegmentKey(segs)
	if !ok {
		tracker.finishCheckpoint(stamp, false)
		return
	}
	// Coverage claims are extent-checked, matching detectSegments: a
	// processed row whose recorded extent no longer matches the
	// recomputed segment is not coverage — its messages belong in
	// the tail.
	processed := make(map[segmentKey]bool, len(registry))
	for _, s := range segs {
		if s.open {
			continue
		}
		row, ok := registry[s.key()]
		if ok && row.State == notebook.SegmentProcessed &&
			row.StartIndex == int64(s.start) && row.EndIndex == int64(s.end) {
			processed[s.key()] = true
		}
	}
	a.spawnCheckpoint(ctx, sessionID, tracker, stamp, notebook.CheckpointRequest{
		TurnNumber:    key.turn,
		SegmentNumber: key.segment,
		Granularity:   notebook.GranularityBoundary,
		RunTag:        runTag,
		// The run-end floor is one gathered event — "context was
		// gathered" — versus the boundary trigger's deeper
		// exploration floor.
		MinExploration: 1,
		Msgs:           uncoveredTail(msgs, segs, processed),
	})
}

// spawnCheckpoint runs checkpoint generation on the same async seam
// as segment generation — detached, bounded, and inline under
// syncSegmentGen so tests are deterministic.
func (a *sessionAgent) spawnCheckpoint(ctx context.Context, sessionID string, tracker *segmentTracker, stamp uint64, req notebook.CheckpointRequest) {
	genMsgs := cloneMessagesForGen(req.Msgs)
	req.Msgs = genMsgs
	genCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), segmentGenTimeout)
	if a.syncSegmentGen {
		a.runCheckpoint(genCtx, sessionID, req, tracker, stamp)
		cancel()
		return
	}
	go func() {
		defer cancel()
		a.runCheckpoint(genCtx, sessionID, req, tracker, stamp)
	}()
}

// runCheckpoint invokes the service and resolves the claim: keep the
// slot on success or a clean not-due (the boundary is evaluated once
// per run), release it on failure so the run-end pass retries.
func (a *sessionAgent) runCheckpoint(ctx context.Context, sessionID string, req notebook.CheckpointRequest, tracker *segmentTracker, stamp uint64) {
	committed, err := a.notebook.GenerateCheckpoint(ctx, sessionID, req)
	if err != nil {
		slog.Error("Failed to generate checkpoint", "session_id", sessionID, "error", err)
		tracker.finishCheckpoint(stamp, true)
		return
	}
	tracker.finishCheckpoint(stamp, false)
	if !committed {
		return
	}
	if a.nbStats != nil {
		stats, _ := a.nbStats.Get(sessionID)
		stats.CheckpointsWritten++
		a.nbStats.Set(sessionID, stats)
	}
	if a.notebookSyncMem0 && a.configStore != nil {
		entries, err := a.notebook.GetByTurnSegment(ctx, sessionID, req.TurnNumber, req.SegmentNumber)
		if err != nil {
			slog.Error("Failed to get checkpoint for mem0 sync", "error", err)
			return
		}
		var fresh []notebook.Entry
		for _, e := range entries {
			if e.EventType == notebook.EventCheckpoint && slices.Contains(e.Tags, req.RunTag) {
				fresh = append(fresh, e)
			}
		}
		// Checkpoint sync is explicit — the only other SyncEntries
		// call site, segment generation, filters these out.
		notebook.NewMem0Sync(a.configStore, a.notebookMemoryServer).SyncEntries(ctx, fresh)
	}
}
