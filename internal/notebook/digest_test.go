package notebook

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// digestEchoGen records GenerateDigest inputs and returns a canned
// entry so tag stamping, title normalization, and the interrupted
// marker can be asserted against a controlled model output.
type digestEchoGen struct {
	inputs []string
	entry  GeneratedEntry
	err    error
}

func (g *digestEchoGen) Generate(_ context.Context, _ string, events []EntryInput) ([]GeneratedEntry, error) {
	entries := make([]GeneratedEntry, len(events))
	for i, ev := range events {
		entries[i] = GeneratedEntry{EventType: ev.EventType, Title: ev.Title, Text: "## " + ev.Title}
	}
	return entries, nil
}

func (g *digestEchoGen) GenerateCheckpoint(context.Context, string, string) (GeneratedEntry, error) {
	return GeneratedEntry{EventType: EventCheckpoint, Title: "Checkpoint", Text: "## Checkpoint"}, nil
}

func (g *digestEchoGen) GenerateDigest(_ context.Context, _ string, input string) (GeneratedEntry, error) {
	if g.err != nil {
		return GeneratedEntry{}, g.err
	}
	g.inputs = append(g.inputs, input)
	e := g.entry
	if e.Title == "" {
		e.Title = "Turn digest"
	}
	if e.Text == "" {
		e.Text = "## Turn digest\n\n" + input
	}
	if e.EventType == "" {
		e.EventType = EventCheckpoint
	}
	return e, nil
}

// digestTurnMsgs builds one turn's messages: a user prompt, an
// assistant message with the given finished calls, then a tool
// message resolving them.
func digestTurnMsgs(calls []message.ToolCall, results []message.ToolResult) []message.Message {
	return []message.Message{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "do it"}}},
		{Role: message.Assistant, Parts: append(
			[]message.ContentPart{message.TextContent{Text: "working"}},
			toolCallParts(calls)...,
		)},
		{Role: message.Tool, Parts: toolResultParts(results)},
	}
}

func toolCallParts(calls []message.ToolCall) []message.ContentPart {
	parts := make([]message.ContentPart, len(calls))
	for i, c := range calls {
		parts[i] = c
	}
	return parts
}

func toolResultParts(results []message.ToolResult) []message.ContentPart {
	parts := make([]message.ContentPart, len(results))
	for i, r := range results {
		parts[i] = r
	}
	return parts
}

func TestGenerateTurnDigest_CommitsGranularityTurn(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := digestTurnMsgs(
		[]message.ToolCall{{ID: "tc1", Name: "edit", Input: `{"file_path":"auth.go"}`, Finished: true}},
		[]message.ToolResult{{ToolCallID: "tc1", Name: "edit", Content: "edited"}},
	)

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    3,
		SegmentNumber: 1,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)
	require.Len(t, gen.inputs, 1)
	require.Contains(t, gen.inputs[0], "Edit auth.go")

	entries, err := svc.GetByTurn(t.Context(), sessionID, 3)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, EventCheckpoint, e.EventType)
	require.Equal(t, GranularityTurn, CheckpointGranularity(e))
	require.Equal(t, int64(1), e.SegmentNumber)
	require.Contains(t, e.Tags, "phase:checkpoint")
	for _, tag := range e.Tags {
		require.NotContains(t, tag, "run:", "turn digests carry no run tag")
	}
}

func TestGenerateTurnDigest_IncludesTrivialEvents(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := digestTurnMsgs(
		[]message.ToolCall{
			{ID: "tc1", Name: "view", Input: `{"file_path":"auth.go"}`, Finished: true},
			{ID: "tc2", Name: "grep", Input: `{"pattern":"login"}`, Finished: true},
		},
		[]message.ToolResult{
			{ToolCallID: "tc1", Name: "view", Content: "package auth"},
			{ToolCallID: "tc2", Name: "grep", Content: "auth.go:12:func login()"},
		},
	)

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)
	require.Len(t, gen.inputs, 1)
	require.Contains(t, gen.inputs[0], "grep", "trivial exploration events feed the digest input")
}

func TestGenerateTurnDigest_TrivialOnlyTurnDigests(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := digestTurnMsgs(
		[]message.ToolCall{{ID: "tc1", Name: "grep", Input: `{"pattern":"login"}`, Finished: true}},
		[]message.ToolResult{{ToolCallID: "tc1", Name: "grep", Content: "auth.go:12:func login()"}},
	)

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed, "a grep-only turn still has evidence worth digesting")
}

func TestGenerateTurnDigest_EmptyTurnSkips(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := []message.Message{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "thanks"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "you're welcome"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		}},
	}

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.False(t, committed)
	require.Empty(t, gen.inputs, "a tool-call-free turn must not cost a model call")
}

func TestGenerateTurnDigest_DedupesPerTurn(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := viewMsgs("tc1")

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    2,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)

	// The same turn again is a clean no-op — before the model call.
	committed, err = svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    2,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.False(t, committed)
	require.Len(t, gen.inputs, 1)

	// A different turn digests normally — the dedup key is the turn.
	committed, err = svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    3,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)
	require.Len(t, gen.inputs, 2)
}

func TestGenerateTurnDigest_CoexistsWithBoundaryCheckpoint(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := viewMsgs("tc1")

	// A boundary checkpoint on the same turn must not suppress the
	// digest — the dedup key is (turn, granularity:turn), not
	// "any checkpoint on this turn".
	ok, err := svc.GenerateCheckpoint(t.Context(), sessionID, CheckpointRequest{
		TurnNumber:     4,
		SegmentNumber:  0,
		Granularity:    GranularityBoundary,
		MinExploration: 1,
		Msgs:           msgs,
	})
	require.NoError(t, err)
	require.True(t, ok)

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    4,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)

	// And the digest must not suppress a later boundary checkpoint —
	// its dedup is the run tag, which digests never carry.
	ok, err = svc.GenerateCheckpoint(t.Context(), sessionID, CheckpointRequest{
		TurnNumber:     4,
		SegmentNumber:  0,
		Granularity:    GranularityBoundary,
		RunTag:         "run:9",
		MinExploration: 1,
		Msgs:           msgs,
	})
	require.NoError(t, err)
	require.True(t, ok)
}

func TestGenerateTurnDigest_InterruptedHeadline(t *testing.T) {
	gen := &digestEchoGen{}
	svc, _, sessionID := newTestService(t, gen)
	msgs := digestTurnMsgs(
		[]message.ToolCall{{ID: "tc1", Name: "edit", Input: `{"file_path":"auth.go"}`, Finished: true}},
		[]message.ToolResult{{ToolCallID: "tc1", Name: "edit", Content: "edited"}},
	)
	// persistCanceledTurn's shape: the turn's last assistant message
	// carries a canceled finish.
	msgs[1].Parts = append(msgs[1].Parts, message.Finish{Reason: message.FinishReasonCanceled})

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    0,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)
	require.Len(t, gen.inputs, 1)
	require.Contains(t, gen.inputs[0], "interrupted")

	entries, err := svc.GetByTurn(t.Context(), sessionID, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Title, "interrupted")
}

func TestGenerateTurnDigest_StripsStructuralModelTags(t *testing.T) {
	gen := &digestEchoGen{entry: GeneratedEntry{
		Title: "Turn 1 digest",
		Text:  "## Turn 1 digest\n\n### Established\n- fact",
		Tags:  []string{"granularity:session", "run:7", "phase:exploration", "file:auth.go"},
	}}
	svc, _, sessionID := newTestService(t, gen)
	msgs := viewMsgs("tc1")

	committed, err := svc.GenerateTurnDigest(t.Context(), sessionID, DigestRequest{
		TurnNumber:    1,
		SegmentNumber: 0,
		Msgs:          msgs,
	})
	require.NoError(t, err)
	require.True(t, committed)

	entries, err := svc.GetByTurn(t.Context(), sessionID, 1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, GranularityTurn, CheckpointGranularity(entries[0]),
		"a hallucinated granularity tag must not misrank the entry")
	require.Contains(t, entries[0].Tags, "phase:checkpoint")
	require.Contains(t, entries[0].Tags, "file:auth.go")
	for _, tag := range entries[0].Tags {
		require.NotContains(t, tag, "run:", "a spurious run tag would falsely dedup boundary checkpoints")
	}
}
