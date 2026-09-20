package cmd

import (
	"database/sql"
	"path/filepath"
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

	insert := func(sessionID string, turnSeq int64, edge, variant, outcome string) {
		t.Helper()
		_, err := queries.InsertEdgeFiring(t.Context(), db.InsertEdgeFiringParams{
			SessionID: sessionID,
			TurnSeq:   turnSeq,
			Edge:      edge,
			Variant:   variant,
			Outcome:   outcome,
		})
		require.NoError(t, err)
	}
	insert(s1.ID, 1, "todos", "", "fired")
	insert(s1.ID, 2, "todos", "", "fired")
	insert(s1.ID, 3, "todos", "", "exhausted")
	insert(s1.ID, 1, "stall", "replan", "fired")
	insert(s1.ID, 2, "stall", "escalate", "fired")
	insert(s1.ID, 3, "stall", "escalate", "gated")
	insert(s2.ID, 1, "todos", "", "fired")

	stats, err := gatherEdgeFiringStats(t.Context(), queries)
	require.NoError(t, err)
	require.Len(t, stats, 5)

	byKey := map[string]EdgeFiringStat{}
	for _, s := range stats {
		byKey[s.Edge+"|"+s.Variant+"|"+s.Outcome] = s
	}
	require.EqualValues(t, 3, byKey["todos||fired"].Firings)
	require.EqualValues(t, 2, byKey["todos||fired"].Sessions)
	require.EqualValues(t, 1, byKey["todos||exhausted"].Firings)
	// The replan-vs-escalate split is the stat the flag-flip decision
	// reads — variants must not collapse into one outcome bucket.
	require.EqualValues(t, 1, byKey["stall|replan|fired"].Firings)
	require.EqualValues(t, 1, byKey["stall|escalate|fired"].Firings)
	require.EqualValues(t, 1, byKey["stall|escalate|gated"].Firings)

	// The merged view across projects dedupes by edge|variant|outcome.
	merged := mergeStats([]ProjectStats{
		{Stats: &Stats{EdgeFirings: stats}},
		{Stats: &Stats{EdgeFirings: []EdgeFiringStat{
			{Edge: "todos", Outcome: "fired", Firings: 5, Sessions: 1},
		}}},
	})
	require.Len(t, merged.EdgeFirings, 5)
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

func TestGatherEdgeFiringStats_UnmigratedDB(t *testing.T) {
	t.Parallel()

	// A project DB from before the edge_firings migration — the
	// missing table must degrade to an empty section, not an error
	// that skips the project's whole stats gather.
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	stats, err := gatherEdgeFiringStats(t.Context(), db.New(conn))
	require.NoError(t, err)
	require.Nil(t, stats)
}

func TestDeriveEdgeSignals(t *testing.T) {
	t.Parallel()

	rows := []EdgeFiringStat{
		{Edge: "stall", Variant: "replan", Outcome: "fired", Firings: 6, Sessions: 4},
		{Edge: "stall", Variant: "escalate", Outcome: "fired", Firings: 2, Sessions: 2},
		{Edge: "burn-watch", Outcome: "fired", Firings: 1, Sessions: 1},
		{Edge: "burn-watch", Outcome: "suppressed", Firings: 24, Sessions: 10},
		{Edge: "burn-watch", Outcome: "gated", Firings: 5, Sessions: 3},
		{Edge: "todos", Outcome: "suppressed", Firings: 12, Sessions: 8},
	}

	signals := deriveEdgeSignals(rows, 10)
	byName := map[string]EdgeSignal{}
	for _, s := range signals {
		byName[s.Name] = s
	}

	// Replan→escalate: 2/(6+2) = 25%.
	require.Equal(t, "25%", byName["replan → escalate"].Value)
	// Burn-watch: 1/30 = 3.3%.
	require.Equal(t, "3.3%", byName["burn-watch rate"].Value)
	// Escalations/session: (2+1)/10 = 0.30.
	require.Equal(t, "0.30", byName["escalations / session"].Value)
	// Gated share: 5/50 = 10%.
	require.Equal(t, "10%", byName["gated share"].Value)
	// Todos never fired — the dead-edge signal names it.
	require.Equal(t, "todos", byName["edges never fired"].Value)
}

func TestDeriveEdgeSignals_Empty(t *testing.T) {
	t.Parallel()

	require.Empty(t, deriveEdgeSignals(nil, 0))
}
