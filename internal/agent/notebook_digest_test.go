package agent

import (
	"fmt"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/stretchr/testify/require"
)

// digestFixture builds n finished turns — each a user prompt, a bash
// call, and its result — and returns the stored list, the index of
// the last turn's user message (the preTurnMsgCount for a run that
// just ended there), and the last assistant message's ID.
func digestFixture(t *testing.T, svc message.Service, sessionID string, n int) ([]message.Message, int, string) {
	t.Helper()
	preTurn := 0
	lastAssistantID := ""
	for i := range n {
		stored, err := svc.List(t.Context(), sessionID)
		require.NoError(t, err)
		preTurn = len(stored)
		mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: fmt.Sprintf("turn %d", i)})
		a := mkMsg(t, svc, sessionID, message.Assistant,
			message.TextContent{Text: "working"},
			message.ToolCall{ID: fmt.Sprintf("tc-%d", i), Name: "bash", Input: `{"command":"ls"}`, Finished: true})
		lastAssistantID = a.ID
		mkMsg(t, svc, sessionID, message.Tool,
			message.ToolResult{ToolCallID: fmt.Sprintf("tc-%d", i), Name: "bash", Content: "out"})
	}
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	return msgs, preTurn, lastAssistantID
}

// digestTurns returns the set of turns carrying a granularity:turn
// checkpoint entry.
func digestTurns(t *testing.T, nb notebook.Service, sessionID string) map[int64]bool {
	t.Helper()
	entries, err := nb.GetEntries(t.Context(), sessionID)
	require.NoError(t, err)
	out := make(map[int64]bool)
	for _, e := range entries {
		if notebook.CheckpointGranularity(e) == notebook.GranularityTurn {
			out[e.TurnNumber] = true
		}
	}
	return out
}

func TestGenerateTurnDigests_FiresForFinishedTurns(t *testing.T) {
	t.Parallel()

	gen := &countingGen{}
	a, svc, nb, sessionID := newSegmentTestAgent(t, gen)
	a.priorTurns = priorTurnsDigest
	a.nbStats = csync.NewMap[string, notebook.Stats]()
	msgs, preTurn, lastAssistantID := digestFixture(t, svc, sessionID, 2)

	a.generateTurnDigests(t.Context(), sessionID, msgs, preTurn, lastAssistantID)

	require.EqualValues(t, 2, gen.digests.Load(), "each finished turn lacking a digest gets one")
	stats, _ := a.nbStats.Get(sessionID)
	require.Equal(t, 2, stats.DigestsWritten)
	got := digestTurns(t, nb, sessionID)
	require.True(t, got[0])
	require.True(t, got[1])
	// The digest keys to its turn's own last segment, not a
	// session-wide position.
	entries, err := nb.GetEntries(t.Context(), sessionID)
	require.NoError(t, err)
	for _, e := range entries {
		if notebook.CheckpointGranularity(e) == notebook.GranularityTurn {
			require.Equal(t, int64(0), e.SegmentNumber, "each turn's tail is its only segment")
		}
	}
}

func TestGenerateTurnDigests_ModeGated(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{priorTurnsStub, priorTurnsVerbatim, ""} {
		gen := &countingGen{}
		a, svc, _, sessionID := newSegmentTestAgent(t, gen)
		a.priorTurns = mode
		msgs, preTurn, lastAssistantID := digestFixture(t, svc, sessionID, 1)
		a.generateTurnDigests(t.Context(), sessionID, msgs, preTurn, lastAssistantID)
		require.Zero(t, gen.digests.Load(), "mode %q must not fire digest generation", mode)
	}
}

func TestGenerateTurnDigests_CatchUpOldestFirstWithCap(t *testing.T) {
	t.Parallel()

	gen := &countingGen{}
	a, svc, nb, sessionID := newSegmentTestAgent(t, gen)
	a.priorTurns = priorTurnsDigest
	msgs, preTurn, lastAssistantID := digestFixture(t, svc, sessionID, digestCatchUpCap+2)

	a.generateTurnDigests(t.Context(), sessionID, msgs, preTurn, lastAssistantID)

	require.EqualValues(t, digestCatchUpCap, gen.digests.Load(),
		"a backlog beyond the cap fills over successive passes")
	got := digestTurns(t, nb, sessionID)
	for i := range int64(digestCatchUpCap) {
		require.True(t, got[i], "oldest turns digest first")
	}
	require.False(t, got[digestCatchUpCap], "turns past the cap wait for the next run")
}

func TestGenerateTurnDigests_SkipsAlreadyDigestedTurn(t *testing.T) {
	t.Parallel()

	gen := &countingGen{}
	a, svc, nb, sessionID := newSegmentTestAgent(t, gen)
	a.priorTurns = priorTurnsDigest
	msgs, preTurn, lastAssistantID := digestFixture(t, svc, sessionID, 2)

	// Seed turn 0's digest — the catch-up pass must not redo it.
	committed, err := nb.GenerateTurnDigest(t.Context(), sessionID, notebook.DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs:          msgs[:3],
	})
	require.NoError(t, err)
	require.True(t, committed)
	require.EqualValues(t, 1, gen.digests.Load())

	a.generateTurnDigests(t.Context(), sessionID, msgs, preTurn, lastAssistantID)

	require.EqualValues(t, 2, gen.digests.Load(), "only turn 1 fires")
	got := digestTurns(t, nb, sessionID)
	require.True(t, got[0])
	require.True(t, got[1])
}

func TestGenerateTurnDigests_SkipsEmptyTurns(t *testing.T) {
	t.Parallel()

	gen := &countingGen{}
	a, svc, nb, sessionID := newSegmentTestAgent(t, gen)
	a.priorTurns = priorTurnsDigest

	// Turn 0 is pure conversation — no tool calls — and turn 1 does
	// real work. The empty turn must not cost a model call.
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "thanks"})
	mkMsg(t, svc, sessionID, message.Assistant, message.TextContent{Text: "welcome"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "now work"})
	a1 := mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-1", Name: "bash", Input: `{"command":"ls"}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-1", Name: "bash", Content: "out"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	a.generateTurnDigests(t.Context(), sessionID, msgs, 2, a1.ID)

	require.EqualValues(t, 1, gen.digests.Load(), "the empty turn's floor skips it before the model call")
	got := digestTurns(t, nb, sessionID)
	require.False(t, got[0])
	require.True(t, got[1])
}

func TestPreparePrompt_DigestModeStubText(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsDigest
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)

	history, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))

	// Digest-mode stub text names the consolidation while keeping
	// the recall pointer — the digest may lag async, so the pointer
	// never depends on it.
	call := renderedCall(t, history, "tc-bash")
	require.JSONEq(t, `{"_collapsed":"prior turn 0 — consolidated in turn digest"}`, call.Input)
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, "consolidated in turn digest")
	require.Contains(t, res, `recall("result:tc-bash")`)

	// Write-class calls keep the re-view-led stub — the args lived
	// in the input and no digest stores them.
	require.Contains(t, renderedResultText(t, history, "tc-shot"), "prior turn 0")
}

func TestSelectNotebookEntries_DigestDemotion(t *testing.T) {
	t.Parallel()

	entries := []notebook.Entry{
		nbSegEntry("r1", 1, 0, 0, notebook.EventFileRead, "read a.go", 10, "file:a.go"),
		nbSegEntry("c1", 1, 0, 1, notebook.EventCommand, "ran ls", 10),
		nbSegEntry("d1", 1, 0, 2, notebook.EventCheckpoint, "digest", 10, "granularity:turn", "phase:checkpoint"),
		nbSegEntry("r2", 2, 0, 0, notebook.EventFileRead, "read b.go", 10, "file:b.go"),
	}

	// With the turn's digest eligible and rendered, its same-turn
	// segment entries demote — the turn's work renders once.
	sel := selectionInput{digestEligible: map[int64]bool{1: true}}
	got, _ := selectNotebookEntries(entries, nil, segmentKey{turn: 3}, sel)
	ids := entryIDs(got)
	require.Contains(t, ids, "d1")
	require.Contains(t, ids, "r2")
	require.NotContains(t, ids, "r1")
	require.NotContains(t, ids, "c1")

	// Without eligibility the digest is just another entry.
	got, _ = selectNotebookEntries(entries, nil, segmentKey{turn: 3}, selectionInput{})
	ids = entryIDs(got)
	require.Contains(t, ids, "d1")
	require.Contains(t, ids, "r1")
	require.Contains(t, ids, "c1")

	// A tags-only digest renders as a bare tag line — it must not
	// strip its turn's real entries.
	thin := slices.Clone(entries)
	thin[2].CompressionLevel = notebook.CompressionTagsOnly
	got, _ = selectNotebookEntries(thin, nil, segmentKey{turn: 3}, sel)
	ids = entryIDs(got)
	require.Contains(t, ids, "d1")
	require.Contains(t, ids, "r1")
	require.Contains(t, ids, "c1")
}

func TestMaybeAutoInject_SkipsDigestCoveredTurn(t *testing.T) {
	t.Parallel()
	agent, q, sessionID := newNotebookTestAgent(t)

	// Turn 1: a compressed read of auth.go whose turn's digest
	// rendered in the prefix — auto-inject must not double it in.
	insertNotebookEntry(t, q, sessionID, 1, notebook.EventFileRead, "DIGESTED CONTENT", 1, "file:auth.go")
	// Turn 2: a compressed read from an undigested turn — injectable.
	insertNotebookEntry(t, q, sessionID, 2, notebook.EventFileRead, "FRESH CONTENT", 1, "file:auth.go")

	msgs := []message.Message{
		{Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "look at internal/auth.go please"},
		}},
	}
	msg := agent.maybeAutoInject(t.Context(), msgs, sessionID, segmentKey{turn: 10}, nil, map[int64]bool{1: true})
	require.NotNil(t, msg)
	var text string
	for _, part := range msg.Content {
		if tp, ok := part.(fantasy.TextPart); ok {
			text += tp.Text
		}
	}
	require.NotContains(t, text, "DIGESTED CONTENT")
	require.Contains(t, text, "FRESH CONTENT")
}

func TestRenderNotebookPrefix_DigestFreezeEligibility(t *testing.T) {
	t.Parallel()

	a, _, nb, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.nbStats = csync.NewMap[string, notebook.Stats]()
	collapse := &turnCollapse{Before: 2}

	// Seed a turn-0 digest.
	committed, err := nb.GenerateTurnDigest(t.Context(), sessionID, notebook.DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs: []message.Message{
			segAssistant("work", message.ToolCall{ID: "tc-0", Name: "bash", Input: `{"command":"ls"}`, Finished: true}),
			segTool(message.ToolResult{ToolCallID: "tc-0", Name: "bash", Content: "out"}),
		},
	})
	require.NoError(t, err)
	require.True(t, committed)

	entries, err := nb.GetEntries(t.Context(), sessionID)
	require.NoError(t, err)

	// The first render freezes eligibility — the digest's turn
	// becomes demotion-eligible for the rest of the run.
	prefix, _ := a.renderNotebookPrefix(t.Context(), sessionID, entries,
		[]message.Message{segUser("go")}, segmentKey{turn: 1, segment: 0},
		segmentKey{turn: 0, segment: 0}, nil, selectionInput{}, collapse)
	require.NotNil(t, collapse.digestTurns)
	require.True(t, collapse.digestTurns[0])
	require.NotEmpty(t, prefix)

	// The render counted the digest under its own granularity, not
	// the consolidated-position counter.
	stats, ok := a.nbStats.Get(sessionID)
	require.True(t, ok)
	require.Equal(t, 1, stats.DigestRenders)
	require.Zero(t, stats.CheckpointRenders)
}
