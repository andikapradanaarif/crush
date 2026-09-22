package agent

import (
	"context"
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
)

// digestCatchUpCap bounds how many turn digests one run-end pass
// fires. First enabling digest mode mid-session leaves every prior
// turn undigested, and an unbounded catch-up would burst small-model
// calls; the remainder fills over successive runs.
const digestCatchUpCap = 4

// claimDigest marks a turn's digest generation in flight — the
// intra-process dedup for run-end passes that run while a previous
// run's digest goroutine is still working. The in-transaction
// (turn, granularity:turn) re-check in GenerateTurnDigest is the
// authoritative dedup; this only avoids a duplicate model call.
func (t *segmentTracker) claimDigest(turn int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.digests[turn] {
		return false
	}
	t.digests[turn] = true
	return true
}

// releaseDigest frees a turn's digest claim once generation resolves.
func (t *segmentTracker) releaseDigest(turn int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.digests, turn)
}

// generateTurnDigests is the run-end digest pass under
// priorTurns=digest: it fires generation for every finished turn
// lacking a granularity:turn entry — catch-up semantics, not just
// the turn this run finished — so a turn whose run aborted (the
// run-end goroutine only runs on the success path) or whose digest
// generation failed digests on a later run. Oldest-first, bounded by
// digestCatchUpCap. There is deliberately no per-turn retry backoff
// like segmentRetryDue's: a failed turn simply refires on the next
// run, bounded by the cap — a once-per-run cadence makes the
// exponential machinery unnecessary. tailTurn bounds ownership the
// same way generateRunEndSegments resolves it: a user message past
// this run's final assistant message means a newer run owns the
// tail. The pass is gated on the mode, not the checkpoint flag —
// notebook_checkpoint owns boundary/session consolidation only.
func (a *sessionAgent) generateTurnDigests(ctx context.Context, sessionID string, msgs []message.Message, preTurnMsgCount int, lastAssistantID string) {
	if a.priorTurns != priorTurnsDigest || a.notebook == nil || sessionID == "" ||
		preTurnMsgCount >= len(msgs) {
		return
	}
	turns := messageTurns(msgs)
	thisTurn := turns[preTurnMsgCount]
	tailTurn := thisTurn
	if lastAssistantID != "" {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].ID == lastAssistantID {
				tailTurn = turns[i]
				break
			}
		}
	}
	entries, err := a.notebook.GetEntries(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to list entries for turn digests", "session_id", sessionID, "error", err)
		return
	}
	digested := make(map[int64]bool)
	for _, e := range entries {
		if notebook.CheckpointGranularity(e) == notebook.GranularityTurn {
			digested[e.TurnNumber] = true
		}
	}
	// lastSeg maps each finished turn to its own last segment — the
	// coverage key the digest is written under. It is deliberately
	// not checkpointSegmentKey's session-wide last closed segment:
	// demotion joins on the entry's TurnNumber, so a digest keyed to
	// an earlier turn would demote the wrong turn's entries.
	segs := segmentBoundaries(msgs, a.segTokenBudget(), a.segMaxSteps())
	lastSeg := make(map[int64]int64)
	for _, s := range segs {
		lastSeg[s.turn] = s.number
	}
	// extent maps each finished turn to its message range — the
	// digest's input is the turn's own messages and nothing else.
	extent := make(map[int64][2]int)
	for i, t := range turns {
		if t > tailTurn {
			break
		}
		e, ok := extent[t]
		if !ok {
			e = [2]int{i, i}
		}
		e[1] = i + 1
		extent[t] = e
	}
	tracker := a.segmentTracker(sessionID)
	fired := 0
	for t := int64(0); t <= tailTurn && fired < digestCatchUpCap; t++ {
		if digested[t] {
			continue
		}
		ext, ok := extent[t]
		if !ok {
			continue
		}
		segNum, ok := lastSeg[t]
		if !ok {
			continue
		}
		// The digest floor, checked before claiming: a turn with no
		// finished tool call can never produce a digest — it would
		// no-op inside GenerateTurnDigest yet still consume a
		// catch-up slot, and without an entry it stays undigested
		// forever, starving every later turn behind a wall of
		// chitchat.
		if !notebook.HasFinishedToolCall(msgs[ext[0]:ext[1]]) {
			continue
		}
		if !tracker.claimDigest(t) {
			continue
		}
		fired++
		a.spawnDigest(ctx, sessionID, tracker, t, notebook.DigestRequest{
			TurnNumber:    t,
			SegmentNumber: segNum,
			Msgs:          msgs[ext[0]:ext[1]],
		})
	}
}

// spawnDigest runs digest generation on the same async seam as
// segment and checkpoint generation — detached, bounded, and inline
// under syncSegmentGen so tests are deterministic.
func (a *sessionAgent) spawnDigest(ctx context.Context, sessionID string, tracker *segmentTracker, turn int64, req notebook.DigestRequest) {
	req.Msgs = cloneMessagesForGen(req.Msgs)
	genCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), segmentGenTimeout)
	if a.syncSegmentGen {
		a.runDigest(genCtx, sessionID, req, tracker, turn)
		cancel()
		return
	}
	a.spawnDetached(func() {
		defer cancel()
		a.runDigest(genCtx, sessionID, req, tracker, turn)
	})
}

// runDigest invokes the service, resolves the in-flight claim, and
// counts the commit. Turn digests never sync to mem0 — they are
// session-internal work logs; SyncEntries additionally filters them
// structurally by granularity.
func (a *sessionAgent) runDigest(ctx context.Context, sessionID string, req notebook.DigestRequest, tracker *segmentTracker, turn int64) {
	defer tracker.releaseDigest(turn)
	committed, err := a.notebook.GenerateTurnDigest(ctx, sessionID, req)
	if err != nil {
		slog.Error("Failed to generate turn digest", "session_id", sessionID, "turn", req.TurnNumber, "error", err)
		return
	}
	if committed && a.nbStats != nil {
		a.nbStats.Update(sessionID, func(s *notebook.Stats) { s.DigestsWritten++ })
	}
}
