package notebook

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// recordingGen captures which inputs the generator sees — plan events
// must bypass it entirely.
type recordingGen struct {
	seen       []EntryInput
	underCount int // return at most this many entries (0 = all)
}

func (g *recordingGen) Generate(_ context.Context, _ string, events []EntryInput) ([]GeneratedEntry, error) {
	g.seen = append(g.seen, events...)
	limit := len(events)
	if g.underCount > 0 && limit > g.underCount {
		limit = g.underCount
	}
	entries := make([]GeneratedEntry, limit)
	for i, ev := range events[:limit] {
		entries[i] = GeneratedEntry{
			EventType: ev.EventType,
			Title:     ev.Title,
			Text:      "## " + ev.Title + "\ncontent",
			Tags:      []string{"tag"},
		}
	}
	return entries, nil
}

func (g *recordingGen) GenerateCheckpoint(context.Context, string, string) (GeneratedEntry, error) {
	return GeneratedEntry{EventType: EventCheckpoint, Title: "Checkpoint", Text: "## Checkpoint"}, nil
}

func (g *recordingGen) GenerateDigest(context.Context, string, string) (GeneratedEntry, error) {
	return GeneratedEntry{EventType: EventCheckpoint, Title: "Turn digest", Text: "## Turn digest"}, nil
}

func todosCallResult(id, metadata string, isError bool) []message.Message {
	return []message.Message{
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: id, Name: "todos", Input: `{"todos":[{"content":"from input","status":"pending"}]}`, Finished: true},
		}},
		{Role: message.Tool, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: id, Name: "todos", Content: "ok", Metadata: metadata, IsError: isError},
		}},
	}
}

func TestBuildPlanEntry(t *testing.T) {
	t.Parallel()
	setupID := "id-of-setup"
	metadata := `{"todos":[` +
		`{"id":"` + setupID + `","key":"setup","content":"set up the harness","status":"completed"},` +
		`{"id":"id-of-test","key":"test","content":"run the checks","status":"pending","depends_on":["` + setupID + `"],"evidence_checks":["verify:build"],"evidence_paths":["internal/agent/"]}` +
		`]}`
	msgs := todosCallResult("tc-t", metadata, false)
	inputs := classifyAll(msgs)
	require.Len(t, inputs, 1)
	require.Equal(t, EventPlan, inputs[0].EventType)

	entry := buildPlanEntry(inputs[0], "")
	require.Equal(t, EventPlan, entry.EventType)
	require.Equal(t, "Plan update", entry.Title)
	require.Contains(t, entry.Text, "1 pending, 0 in progress, 1 completed")
	require.Contains(t, entry.Text, "[completed] set up the harness (key: setup)")
	// Dep edges render as item keys, not minted ids.
	require.Contains(t, entry.Text, "depends_on: setup")
	require.NotContains(t, entry.Text, setupID)
	require.Contains(t, entry.Text, "checks: verify:build")
	require.Contains(t, entry.Text, "paths: internal/agent/")
	require.Contains(t, entry.Tags, "plan")
	require.Contains(t, entry.Tags, "file:internal/agent")
	require.Contains(t, entry.Tags, "file:agent")
}

func TestBuildPlanEntryFallsBackToCallInput(t *testing.T) {
	t.Parallel()
	input := EntryInput{
		ToolCall: &message.ToolCall{ID: "tc-t", Name: "todos",
			Input: `{"todos":[{"content":"from input","status":"pending","key":"a","depends_on":[],"evidence_paths":["x.go"]}]}`, Finished: true},
		EventType: EventPlan,
	}
	entry := buildPlanEntry(input, "")
	require.Contains(t, entry.Text, "[pending] from input")
	require.Contains(t, entry.Tags, "file:x.go")
}

func TestClassifyPlanCalls(t *testing.T) {
	t.Parallel()

	t.Run("landed plan write is a plan event", func(t *testing.T) {
		t.Parallel()
		inputs := classifyAll(todosCallResult("tc-t", `{"todos":[{"content":"x","status":"pending"}]}`, false))
		require.Len(t, inputs, 1)
		require.Equal(t, EventPlan, inputs[0].EventType)
	})

	t.Run("rejected plan write stays a general event", func(t *testing.T) {
		t.Parallel()
		inputs := classifyAll(todosCallResult("tc-t", "", true))
		require.Len(t, inputs, 1)
		require.Equal(t, EventGeneral, inputs[0].EventType)
	})

	t.Run("interrupted plan write stays a general event", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			{Role: message.Assistant, Parts: []message.ContentPart{
				message.ToolCall{ID: "tc-t", Name: "todos", Input: `{"todos":[]}`, Finished: true},
			}},
		}
		inputs := classifyAll(msgs)
		require.Len(t, inputs, 1)
		require.Equal(t, EventGeneral, inputs[0].EventType)
	})
}

func TestGenerateSegmentEntries_PlanEntryBypassesGenerator(t *testing.T) {
	gen := &recordingGen{}
	svc, _, sessionID := newSegmentTestService(t, gen)
	ctx := context.Background()

	msgs := append(segmentEditMsgs(), todosCallResult("tc-t",
		`{"todos":[{"id":"i1","key":"fix","content":"fix the gate","status":"in_progress","evidence_paths":["internal/agent/scope_gate.go"]}]}`, false)...)
	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 0, 0, 4, msgs))

	entries, err := svc.GetEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	var planEntry, editEntry *Entry
	for i := range entries {
		switch entries[i].EventType {
		case EventPlan:
			planEntry = &entries[i]
		case EventFileEdit:
			editEntry = &entries[i]
		}
	}
	require.NotNil(t, planEntry)
	require.NotNil(t, editEntry)
	require.Contains(t, planEntry.EntryText, "[in_progress] fix the gate")
	require.Contains(t, planEntry.EntryText, "paths: internal/agent/scope_gate.go")

	// The plan event never reaches the generator — only the edit did.
	require.Len(t, gen.seen, 1)
	require.Equal(t, EventFileEdit, gen.seen[0].EventType)
}

func TestGenerateSegmentEntries_GeneratorUnderProduction(t *testing.T) {
	// The generator merged two inputs into one entry — the uncovered
	// input must get a fallback entry, not a stored zero value.
	gen := &recordingGen{underCount: 1}
	svc, _, sessionID := newSegmentTestService(t, gen)
	ctx := context.Background()

	msgs := append(segmentEditMsgs(), todosCallResult("tc-t",
		`{"todos":[{"id":"i1","content":"plan thing","status":"pending","evidence_paths":["x.go"]}]}`, false)...)
	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 0, 0, 4, msgs))

	entries, err := svc.GetEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.NotEmpty(t, e.Title)
		require.NotEmpty(t, e.EntryText)
		require.NotEmpty(t, e.EventType)
	}
}
