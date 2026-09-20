package notebook

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func mem0Item(memory, workDir, sessionID, eventType string, turn int64, tags []string) map[string]any {
	tagList := make([]any, len(tags))
	for i, t := range tags {
		tagList[i] = t
	}
	return map[string]any{
		"memory": memory,
		"metadata": map[string]any{
			"working_dir":  workDir,
			"session_id":   sessionID,
			"event_type":   eventType,
			"turn_number":  float64(turn),
			"event_number": float64(1),
			"tags":         tagList,
			"compression":  float64(0),
		},
	}
}

func TestBuildHydrationSeeds_TierOrdering(t *testing.T) {
	t.Parallel()
	dir := "/repo/a"
	items := []map[string]any{
		mem0Item("## Decision\nchose sqlite", dir, "s0", EventDecision, 9, []string{"type:decision"}),
		mem0Item("## Checkpoint\nestablished things", dir, "s0", EventCheckpoint, 9, []string{"granularity:session"}),
		mem0Item("## Read auth.go\nmiddleware notes", dir, "s0", EventFileRead, 8, []string{"file:auth.go"}),
		mem0Item("## Cmd\nran tests", dir, "s0", EventCommand, 10, nil),
	}
	seeds := BuildHydrationSeeds(items, HydrationSeedMaxTokens)
	require.Len(t, seeds, 4)
	// Checkpoint (consolidated position) first, file-tagged (pinned
	// proxy) second, decision third, rest last.
	require.Equal(t, "Checkpoint", seeds[0].Title)
	require.Equal(t, "Read auth.go", seeds[1].Title)
	require.Equal(t, "Decision", seeds[2].Title)
	require.Equal(t, "Cmd", seeds[3].Title)
	for _, s := range seeds {
		require.Contains(t, s.Tags, TagHydrated)
		require.Contains(t, s.Tags, "origin:s0")
	}
	// Granularity tags survive — a hydrated checkpoint stays eligible
	// for LatestCheckpointIDs.
	require.Contains(t, seeds[0].Tags, "granularity:session")
}

func TestBuildHydrationSeeds_StripsRecallablePrefixes(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		mem0Item("## Note\nsee turn:5 and segment:5.2, result:tc-9 for details", "/repo/a", "s0", EventGeneral, 3, nil),
	}
	seeds := BuildHydrationSeeds(items, HydrationSeedMaxTokens)
	require.Len(t, seeds, 1)
	require.Contains(t, seeds[0].Text, "origin-turn:5")
	require.Contains(t, seeds[0].Text, "origin-segment:5.2")
	require.Contains(t, seeds[0].Text, "origin-result:tc-9")
	require.NotContains(t, seeds[0].Text, " turn:5")
	require.NotContains(t, seeds[0].Text, " result:tc-9")
	// The origin date rides in the text — RenderEntries emits no
	// CreatedAt.
	require.Contains(t, seeds[0].Text, "Seeded from an earlier session")
}

func TestBuildHydrationSeeds_TokenBudget(t *testing.T) {
	t.Parallel()
	var items []map[string]any
	for i := range 20 {
		items = append(items, mem0Item(fmt.Sprintf("## E%d\n%s", i, strings.Repeat("x", 400)), "/repo/a", "s0", EventCommand, int64(i), nil))
	}
	seeds := BuildHydrationSeeds(items, 200)
	require.NotEmpty(t, seeds)
	var total int64
	for _, s := range seeds {
		total += estimateTokens(s.Text)
	}
	require.LessOrEqual(t, total, int64(200))
}

func TestBuildHydrationSeeds_SkipsEmptyAndForeignShapes(t *testing.T) {
	t.Parallel()
	items := []map[string]any{
		{"memory": ""},
		{"nope": true},
		mem0Item("## Kept\nbody", "/repo/a", "s0", EventGeneral, 1, nil),
	}
	seeds := BuildHydrationSeeds(items, HydrationSeedMaxTokens)
	require.Len(t, seeds, 1)
	require.Equal(t, "Kept", seeds[0].Title)
}

func TestSeedEntries_WritesAndDeduplicates(t *testing.T) {
	t.Parallel()
	svc, _, sessionID := newTestService(t, &mockGenerator{})
	ctx := context.Background()

	seeds := BuildHydrationSeeds([]map[string]any{
		mem0Item("## Read auth.go\nmiddleware notes", "/repo/a", "s0", EventFileRead, 5, []string{"file:auth.go"}),
	}, HydrationSeedMaxTokens)
	require.Len(t, seeds, 1)
	seeded, err := svc.SeedEntries(ctx, sessionID, seeds)
	require.NoError(t, err)
	require.True(t, seeded)

	entries, err := svc.GetEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, HydrationTurnNumber, e.TurnNumber)
	require.Equal(t, int64(0), e.SegmentNumber)
	require.Equal(t, "Read auth.go", e.Title)
	require.NotEmpty(t, e.EntryTextFull, "recall drill-down needs the full text")
	require.True(t, e.Succeeded)

	// The marker is the seeds themselves — a second call is a no-op.
	seeded, err = svc.SeedEntries(ctx, sessionID, seeds)
	require.NoError(t, err)
	require.False(t, seeded)
	entries, err = svc.GetEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// Seeds never enter coverage accounting as a real turn — the
	// sentinel key is all backfill sees, and no segment lives there.
	// (TagHydrated is what keeps them out of SyncEntries.)
	turns, err := svc.TurnsWithEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, map[int64]bool{HydrationTurnNumber: true}, turns)
}

func TestSeedEntries_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	svc, _, sessionID := newTestService(t, &mockGenerator{})
	seeded, err := svc.SeedEntries(context.Background(), sessionID, nil)
	require.NoError(t, err)
	require.False(t, seeded)
}

func TestSessionCounter_RoundTrip(t *testing.T) {
	t.Parallel()
	svc, _, sessionID := newTestService(t, &mockGenerator{})
	ctx := context.Background()
	n, err := svc.SessionCounter(ctx, sessionID, CounterHydrationAttempts)
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, svc.BumpSessionCounter(ctx, sessionID, CounterHydrationAttempts, 2))
	n, err = svc.SessionCounter(ctx, sessionID, CounterHydrationAttempts)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

func TestFetchHydrationMemories_PartitionedAndUncapped(t *testing.T) {
	dirA := t.TempDir()
	cfg := mem0TestStore(t, dirA)
	stubSearchCaps(t, mem0SearchCaps{limitArg: "limit"})
	workDir := canonicalizeWorkingDir(dirA)

	var sb strings.Builder
	sb.WriteString(`{"results":[`)
	for i := range mem0SearchTopK + 5 {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"memory":"m%d","metadata":{"working_dir":%q}}`, i, workDir)
	}
	fmt.Fprintf(&sb, `,{"memory":"foreign","metadata":{"working_dir":"/other"}}]}`)
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, toolName, _ string) (mcp.ToolResult, error) {
		require.Equal(t, "search_memories", toolName)
		return mcp.ToolResult{Type: "text", Content: sb.String()}, nil
	})

	items, err := FetchHydrationMemories(context.Background(), cfg, "mem0")
	require.NoError(t, err)
	// The wide window survives: hydration must not inherit the
	// model-facing top_k cap.
	require.Len(t, items, mem0SearchTopK+5)
	for _, it := range items {
		require.NotEqual(t, "foreign", it["memory"])
	}
}

func TestFetchHydrationMemories_PrefersListTool(t *testing.T) {
	dirA := t.TempDir()
	cfg := mem0TestStore(t, dirA)
	workDir := canonicalizeWorkingDir(dirA)

	orig := mem0ListTool
	mem0ListTool = func(string) string { return "get_all_memories" }
	t.Cleanup(func() { mem0ListTool = orig })

	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, toolName, _ string) (mcp.ToolResult, error) {
		require.Equal(t, "get_all_memories", toolName)
		return mcp.ToolResult{Type: "text", Content: fmt.Sprintf(
			`[{"memory":"all","metadata":{"working_dir":%q}}]`, workDir)}, nil
	})
	items, err := FetchHydrationMemories(context.Background(), cfg, "mem0")
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestFetchHydrationMemories_UnparseableIsEmpty(t *testing.T) {
	dirA := t.TempDir()
	cfg := mem0TestStore(t, dirA)
	stubSearchCaps(t, mem0SearchCaps{filters: true})
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _, _ string) (mcp.ToolResult, error) {
		// Even server-filtered, an unparseable response yields
		// nothing to seed — hydration never inherits the recall
		// path's fail-open prose.
		return mcp.ToolResult{Type: "text", Content: "3 memories found, trust me"}, nil
	})
	items, err := FetchHydrationMemories(context.Background(), cfg, "mem0")
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestFetchHydrationMemories_NoServerNoWorkDir(t *testing.T) {
	items, err := FetchHydrationMemories(context.Background(), nil, "mem0")
	require.NoError(t, err)
	require.Empty(t, items)
	cfg := mem0TestStore(t, "")
	items, err = FetchHydrationMemories(context.Background(), cfg, "mem0")
	require.NoError(t, err)
	require.Empty(t, items)
}
