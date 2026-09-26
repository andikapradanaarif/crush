package notebook

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// countingGenerator counts Generator calls so policy tests can assert
// the sidecar LLM never ran.
type countingGenerator struct {
	calls atomic.Int64
}

func (g *countingGenerator) Generate(_ context.Context, _ string, events []EntryInput) ([]GeneratedEntry, error) {
	g.calls.Add(1)
	entries := make([]GeneratedEntry, len(events))
	for i, ev := range events {
		entries[i] = GeneratedEntry{
			EventType: ev.EventType,
			Title:     ev.Title,
			Text:      "## " + ev.Title + "\ngenerated",
			Tags:      defaultTagsForEvent(ev, ""),
		}
	}
	return entries, nil
}

func (g *countingGenerator) GenerateCheckpoint(_ context.Context, _ string, input string) (GeneratedEntry, error) {
	g.calls.Add(1)
	return GeneratedEntry{EventType: EventCheckpoint, Title: "Checkpoint", Text: "## Checkpoint\n\n" + input}, nil
}

func (g *countingGenerator) GenerateDigest(_ context.Context, _ string, input string) (GeneratedEntry, error) {
	g.calls.Add(1)
	return GeneratedEntry{EventType: EventCheckpoint, Title: "Turn digest", Text: "## Turn digest\n\n" + input}, nil
}

func newPolicyService(t *testing.T, gen Generator, policy string) (Service, string) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	sessionID := uuid.New().String()
	_, err = q.CreateSession(context.Background(), db.CreateSessionParams{
		ID:    sessionID,
		Title: "test",
	})
	require.NoError(t, err)
	svc := NewService(q, gen, Options{
		MaxEntryTokens:    1000,
		MaxNotebookTokens: 100000,
		DB:                conn,
		GeneratePolicy:    policy,
	})
	return svc, sessionID
}

func TestGeneratePolicy_NeverSkipsLLM(t *testing.T) {
	gen := &countingGenerator{}
	svc, sessionID := newPolicyService(t, gen, GenerateNever)
	ctx := context.Background()

	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 0, 0, 2, segmentEditMsgs()))

	// The entry still commits — deterministic fallback, not silence.
	entries, err := svc.GetEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Zero(t, gen.calls.Load())
}

func TestGeneratePolicy_UnderPressure(t *testing.T) {
	gen := &countingGenerator{}
	svc, sessionID := newPolicyService(t, gen, GenerateUnderPressure)
	ctx := context.Background()

	// Not pressured: deterministic fallback, no generator call.
	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 0, 0, 2, segmentEditMsgs()))
	require.Zero(t, gen.calls.Load())

	svc.SetPressure(sessionID, true)
	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 1, 2, 4, segmentEditMsgs()))
	require.Equal(t, int64(1), gen.calls.Load())

	svc.SetPressure(sessionID, false)
	require.NoError(t, svc.GenerateSegmentEntries(ctx, sessionID, 0, 2, 4, 6, segmentEditMsgs()))
	require.Equal(t, int64(1), gen.calls.Load())
}

func TestGeneratePolicy_NeverSkipsCheckpointAndDigest(t *testing.T) {
	gen := &countingGenerator{}
	svc, sessionID := newPolicyService(t, gen, GenerateNever)
	ctx := context.Background()

	committed, err := svc.GenerateCheckpoint(ctx, sessionID, CheckpointRequest{
		TurnNumber:     1,
		SegmentNumber:  1,
		Granularity:    GranularityBoundary,
		MinExploration: 1,
		Msgs:           viewMsgs("tc1"),
	})
	require.NoError(t, err)
	require.False(t, committed)

	committed, err = svc.GenerateTurnDigest(ctx, sessionID, DigestRequest{
		TurnNumber:    1,
		SegmentNumber: 1,
		Msgs:          segmentEditMsgs(),
	})
	require.NoError(t, err)
	require.False(t, committed)

	require.Zero(t, gen.calls.Load())
}

func TestGeneratePolicy_UnderPressureCheckpointWaitsForLatch(t *testing.T) {
	gen := &countingGenerator{}
	svc, sessionID := newPolicyService(t, gen, GenerateUnderPressure)
	ctx := context.Background()
	req := CheckpointRequest{
		TurnNumber:     1,
		SegmentNumber:  1,
		Granularity:    GranularityBoundary,
		MinExploration: 1,
		Msgs:           viewMsgs("tc1"),
	}

	committed, err := svc.GenerateCheckpoint(ctx, sessionID, req)
	require.NoError(t, err)
	require.False(t, committed)
	require.Zero(t, gen.calls.Load())

	svc.SetPressure(sessionID, true)
	committed, err = svc.GenerateCheckpoint(ctx, sessionID, req)
	require.NoError(t, err)
	require.True(t, committed)
	require.Equal(t, int64(1), gen.calls.Load())
}
