package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestGatherPruningStats(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries)

	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	big := strings.Repeat("x", 1000)
	mk := func(role message.MessageRole, parts ...message.ContentPart) {
		t.Helper()
		_, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: parts,
		})
		require.NoError(t, err)
	}

	mk(message.Assistant, message.ToolCall{ID: "tc-1", Name: "view", Finished: true})
	mk(message.Tool, message.ToolResult{
		ToolCallID: "tc-1", Name: "view", Content: big,
		Superseded: &message.SupersededMark{Path: "a.go", ByTool: "edit", Turn: 1, Applied: true},
	})
	mk(message.Tool, message.ToolResult{
		ToolCallID: "tc-2", Name: "bash", Content: big,
		Superseded: &message.SupersededMark{Turn: 2, Applied: true, Kind: message.StubKindStale},
	})
	// Pending marks are not counted — nothing rendered yet.
	mk(message.Tool, message.ToolResult{
		ToolCallID: "tc-3", Name: "view", Content: big,
		Superseded: &message.SupersededMark{Path: "b.go", ByTool: "edit", Turn: 3},
	})
	// Plain results are not counted.
	mk(message.Tool, message.ToolResult{ToolCallID: "tc-4", Name: "view", Content: big})

	stats, err := gatherPruningStats(t.Context(), queries)
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.EqualValues(t, 2, stats.StubbedResults)
	require.EqualValues(t, 1, stats.Sessions)
	require.Positive(t, stats.SavedBytes)
	require.Less(t, stats.SavedBytes, int64(2*len(big)))

	kinds := make(map[string]PruningKindStats)
	for _, k := range stats.ByKind {
		kinds[k.Kind] = k
	}
	require.EqualValues(t, 1, kinds["superseded"].Results)
	require.EqualValues(t, 1, kinds["stale"].Results)
}

func TestGatherStats_ProjectIndex(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries)

	mk := func(sessID string, name string) {
		t.Helper()
		_, err := messages.Create(t.Context(), sessID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.ToolCall{ID: "tc-" + name, Name: name, Finished: true}},
		})
		require.NoError(t, err)
	}

	s1, err := sessions.Create(t.Context(), "one")
	require.NoError(t, err)
	s2, err := sessions.Create(t.Context(), "two")
	require.NoError(t, err)

	mk(s1.ID, "map")
	mk(s1.ID, "map")
	mk(s2.ID, "map")
	mk(s2.ID, "view")

	stats, err := gatherStats(t.Context(), conn)
	require.NoError(t, err)
	require.NotNil(t, stats.ProjectIndex)
	require.EqualValues(t, 3, stats.ProjectIndex.MapCalls)
	require.EqualValues(t, 2, stats.ProjectIndex.Sessions)
}

func TestGatherPruningStats_Empty(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	stats, err := gatherPruningStats(t.Context(), db.New(conn))
	require.NoError(t, err)
	require.Nil(t, stats)
}

func TestGatherEdgeFiringStats(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	queries := db.New(conn)
	sessions := session.NewService(queries, conn)
	s1, err := sessions.Create(t.Context(), "one")
	require.NoError(t, err)
	s2, err := sessions.Create(t.Context(), "two")
	require.NoError(t, err)

	insert := func(sessionID string, turnSeq int64, edge, outcome string) {
		t.Helper()
		_, err := queries.InsertEdgeFiring(t.Context(), db.InsertEdgeFiringParams{
			SessionID: sessionID,
			TurnSeq:   turnSeq,
			Edge:      edge,
			Outcome:   outcome,
		})
		require.NoError(t, err)
	}
	insert(s1.ID, 1, "todos", "fired")
	insert(s1.ID, 2, "todos", "fired")
	insert(s1.ID, 3, "todos", "exhausted")
	insert(s1.ID, 1, "stall", "gated")
	insert(s2.ID, 1, "todos", "fired")

	stats, err := gatherEdgeFiringStats(t.Context(), queries)
	require.NoError(t, err)
	require.Len(t, stats, 3)

	byKey := map[string]EdgeFiringStat{}
	for _, s := range stats {
		byKey[s.Edge+"|"+s.Outcome] = s
	}
	require.EqualValues(t, 3, byKey["todos|fired"].Firings)
	require.EqualValues(t, 2, byKey["todos|fired"].Sessions)
	require.EqualValues(t, 1, byKey["todos|exhausted"].Firings)
	require.EqualValues(t, 1, byKey["stall|gated"].Firings)

	// The merged view across projects dedupes by edge|outcome.
	merged := mergeStats([]ProjectStats{
		{Stats: &Stats{EdgeFirings: stats}},
		{Stats: &Stats{EdgeFirings: []EdgeFiringStat{
			{Edge: "todos", Outcome: "fired", Firings: 5, Sessions: 1},
		}}},
	})
	require.Len(t, merged.EdgeFirings, 3)
	var fired EdgeFiringStat
	for _, e := range merged.EdgeFirings {
		if e.Edge == "todos" && e.Outcome == "fired" {
			fired = e
		}
	}
	require.EqualValues(t, 8, fired.Firings)
	require.EqualValues(t, 3, fired.Sessions)
}

func TestGatherEdgeFiringStats_Empty(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	stats, err := gatherEdgeFiringStats(t.Context(), db.New(conn))
	require.NoError(t, err)
	require.Nil(t, stats)
}
