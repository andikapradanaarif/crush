package notebook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// mem0TestStore builds a ConfigStore rooted at workDir, isolated from
// the developer's real global config. No t.Parallel(): it sets env vars.
func mem0TestStore(t *testing.T, workDir string) *config.ConfigStore {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))
	store, err := config.Load(workDir, t.TempDir(), false)
	require.NoError(t, err)
	return store
}

// stubRunMCPTool swaps the MCP round-trip for a canned implementation.
// No t.Parallel(): it mutates a package global.
func stubRunMCPTool(t *testing.T, fn func(ctx context.Context, cfg *config.ConfigStore, name, toolName, input string) (mcp.ToolResult, error)) {
	t.Helper()
	orig := runMCPTool
	runMCPTool = fn
	t.Cleanup(func() { runMCPTool = orig })
}

// stubFilterCapable forces the search_memories filters capability
// check. No t.Parallel(): it mutates a package global.
func stubFilterCapable(t *testing.T, capable bool) {
	t.Helper()
	orig := mem0SearchFilterCapable
	mem0SearchFilterCapable = func(string) bool { return capable }
	t.Cleanup(func() { mem0SearchFilterCapable = orig })
}

func TestSearchMem0_PartitionsByWorkingDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	cfgA := mem0TestStore(t, dirA)

	// Metadata carries the normalized spelling — the same key
	// SyncEntries writes.
	payload := fmt.Sprintf(`{"results":[
		{"id":"1","memory":"dir A fact","metadata":{"working_dir":%q,"session_id":"sA"}},
		{"id":"2","memory":"dir B secret","metadata":{"working_dir":%q,"session_id":"sB"}},
		{"id":"3","memory":"legacy memory","metadata":{"session_id":"sOld"}},
		{"id":"4","memory":"malformed","metadata":"not-an-object"}
	]}`, normalizeWorkingDir(dirA), normalizeWorkingDir(dirB))

	var gotTool, gotInput string
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, name, toolName, input string) (mcp.ToolResult, error) {
		gotTool, gotInput = toolName, input
		return mcp.ToolResult{Type: "text", Content: payload}, nil
	})

	result, err := SearchMem0(context.Background(), cfgA, "mem0", "fact")
	require.NoError(t, err)
	require.Equal(t, "search_memories", gotTool)
	require.Contains(t, result, "dir A fact")
	require.NotContains(t, result, "dir B secret")
	require.NotContains(t, result, "legacy memory")
	require.NotContains(t, result, "malformed")

	// With no filters-capable server registered, the request
	// over-fetches so the client-side partition doesn't drop
	// same-project memories that didn't rank in a global top-10.
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotInput), &args))
	require.Equal(t, mem0AgentID, args["agent_id"])
	require.Equal(t, float64(mem0SearchFetchK), args["top_k"])
}

func TestSearchMem0_ServerSideFilter(t *testing.T) {
	workDir := t.TempDir()
	cfg := mem0TestStore(t, workDir)
	stubFilterCapable(t, true)

	var gotInput string
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _, input string) (mcp.ToolResult, error) {
		gotInput = input
		// Not per-memory JSON — the server-side filter is trusted, so
		// the raw response is returned rather than dropped.
		return mcp.ToolResult{Type: "text", Content: "Found 2 memories:\n- a\n- b"}, nil
	})

	result, err := SearchMem0(context.Background(), cfg, "mem0", "fact")
	require.NoError(t, err)
	require.Contains(t, result, "Found 2 memories")

	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotInput), &args))
	require.Equal(t, float64(mem0SearchTopK), args["top_k"])
	require.Equal(t, map[string]any{
		"AND": []any{
			map[string]any{"agent_id": mem0AgentID},
			map[string]any{"metadata.working_dir": normalizeWorkingDir(workDir)},
		},
	}, args["filters"])
}

func TestSearchMem0_UnparseableUnfilteredReturnsEmpty(t *testing.T) {
	cfg := mem0TestStore(t, t.TempDir())

	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _, _ string) (mcp.ToolResult, error) {
		// Plain-text response carrying a foreign memory; with no
		// server-side filter available the partition can't be proven.
		return mcp.ToolResult{Type: "text", Content: "Found memories: other repo's secret"}, nil
	})

	result, err := SearchMem0(context.Background(), cfg, "mem0", "anything")
	require.NoError(t, err)
	require.Equal(t, "", result, "unverifiable results must not leak cross-project memories")
}

func TestSearchMem0_PropagatesToolError(t *testing.T) {
	cfg := mem0TestStore(t, t.TempDir())

	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _, _ string) (mcp.ToolResult, error) {
		return mcp.ToolResult{}, fmt.Errorf("server unreachable")
	})

	_, err := SearchMem0(context.Background(), cfg, "mem0", "anything")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mem0 search failed")
}

func TestSyncEntries_WorkingDirMetadata(t *testing.T) {
	workDir := t.TempDir()
	cfg := mem0TestStore(t, workDir)

	var inputs []string
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, name, toolName, input string) (mcp.ToolResult, error) {
		require.Equal(t, "mem0", name)
		require.Equal(t, "add_memory", toolName)
		inputs = append(inputs, input)
		return mcp.ToolResult{Type: "text", Content: "ok"}, nil
	})

	m := NewMem0Sync(cfg, "mem0", "session1")
	m.SyncEntries(context.Background(), []Entry{
		{
			ID:          "e1",
			SessionID:   "session1",
			Title:       "Read auth.go",
			EntryText:   "read auth middleware",
			EventType:   EventFileRead,
			TurnNumber:  3,
			EventNumber: 1,
			Tags:        []string{"file:auth.go"},
		},
		{
			// Hydrated seed — must never re-sync or it would compound
			// one copy of the origin memory per hydrated session.
			ID:        "e2",
			SessionID: "session1",
			Title:     "Seed",
			EntryText: "hydrated from prior session",
			Tags:      []string{TagHydrated, "origin:session0"},
		},
	})

	require.Len(t, inputs, 1, "hydrated entries must not re-sync")
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(inputs[0]), &args))
	require.Equal(t, mem0AgentID, args["agent_id"])
	metadata, ok := args["metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, normalizeWorkingDir(workDir), metadata["working_dir"])
	require.Equal(t, "session1", metadata["session_id"])
	require.Equal(t, "session1", metadata["origin_session_id"])
}

func TestSyncEntries_SkipsWhenWorkingDirUnknown(t *testing.T) {
	cfg := mem0TestStore(t, "")

	called := false
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _, _ string) (mcp.ToolResult, error) {
		called = true
		return mcp.ToolResult{Type: "text", Content: "ok"}, nil
	})

	NewMem0Sync(cfg, "mem0", "session1").SyncEntries(context.Background(), []Entry{{ID: "e1"}})
	require.False(t, called, "entries without a partition key must not sync")
}

func TestNormalizeWorkingDir(t *testing.T) {
	dir := t.TempDir()

	// A symlinked spelling resolves to the same partition.
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	require.Equal(t, normalizeWorkingDir(dir), normalizeWorkingDir(link))

	// On case-insensitive filesystems a case-variant spelling aliases.
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		require.Equal(t, normalizeWorkingDir(dir), normalizeWorkingDir(strings.ToUpper(dir)))
		require.Equal(t, strings.ToLower(normalizeWorkingDir(dir)), normalizeWorkingDir(dir))
	}
}

func TestFilterMem0Results(t *testing.T) {
	const dirA = "/repo/a"

	tests := []struct {
		name    string
		content string
		want    []string // substrings expected in the output
		notWant []string
		ok      bool
	}{
		{
			name:    "bare array keeps same-dir only",
			content: `[{"memory":"a1","metadata":{"working_dir":"/repo/a"}},{"memory":"b1","metadata":{"working_dir":"/repo/b"}}]`,
			want:    []string{"a1"},
			notWant: []string{"b1"},
			ok:      true,
		},
		{
			name:    "results wrapper",
			content: `{"results":[{"memory":"a1","metadata":{"working_dir":"/repo/a"}}]}`,
			want:    []string{"a1"},
			ok:      true,
		},
		{
			name:    "memories wrapper",
			content: `{"memories":[{"memory":"a1","metadata":{"working_dir":"/repo/a"}}]}`,
			want:    []string{"a1"},
			ok:      true,
		},
		{
			name:    "single object with metadata",
			content: `{"memory":"a1","metadata":{"working_dir":"/repo/a"}}`,
			want:    []string{"a1"},
			ok:      true,
		},
		{
			name:    "missing working_dir excluded",
			content: `[{"memory":"a1","metadata":{"session_id":"s1"}}]`,
			notWant: []string{"a1"},
			ok:      true,
		},
		{
			name:    "missing metadata excluded",
			content: `[{"memory":"a1"}]`,
			notWant: []string{"a1"},
			ok:      true,
		},
		{
			name:    "non-string working_dir excluded",
			content: `[{"memory":"a1","metadata":{"working_dir":42}}]`,
			notWant: []string{"a1"},
			ok:      true,
		},
		{
			name:    "empty working_dir excluded",
			content: `[{"memory":"a1","metadata":{"working_dir":""}}]`,
			notWant: []string{"a1"},
			ok:      true,
		},
		{
			name:    "empty results parses",
			content: `{"results":[]}`,
			ok:      true,
		},
		{
			name:    "non-JSON is not ok",
			content: "Found memories: stuff",
			ok:      false,
		},
		{
			name:    "JSON without memory list is not ok",
			content: `{"detail":"nope"}`,
			ok:      false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := filterMem0Results(tc.content, dirA, "mem0")
			require.Equal(t, tc.ok, ok)
			for _, w := range tc.want {
				require.Contains(t, out, w)
			}
			for _, nw := range tc.notWant {
				require.NotContains(t, out, nw)
			}
		})
	}

	// Foreign-only results keep nothing.
	out, ok := filterMem0Results(`[{"memory":"b1","metadata":{"working_dir":"/repo/b"}}]`, dirA, "mem0")
	require.True(t, ok)
	require.Equal(t, "", out)
}

func TestFilterMem0Results_SortsByScoreAndCaps(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := range mem0SearchTopK + 5 {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"memory":"m%d","score":%d,"metadata":{"working_dir":"/repo/a"}}`, i, i)
	}
	sb.WriteString(`,{"memory":"foreign","score":9999,"metadata":{"working_dir":"/repo/b"}}]`)

	out, ok := filterMem0Results(sb.String(), "/repo/a", "mem0")
	require.True(t, ok)

	var kept []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &kept))
	require.Len(t, kept, mem0SearchTopK, "results are capped at the model-facing top_k")
	// The high-scoring foreign memory was excluded before ranking.
	for _, m := range kept {
		require.NotEqual(t, "foreign", m["memory"])
	}
	// Survivors rank by score descending.
	for i := range len(kept) - 1 {
		require.GreaterOrEqual(t, mem0Score(kept[i]), mem0Score(kept[i+1]))
	}
	require.Equal(t, "m14", kept[0]["memory"], "highest-scoring same-dir memory ranks first")
}

func TestSchemaHasProperty(t *testing.T) {
	require.True(t, schemaHasProperty(map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{}, "filters": map[string]any{}},
	}, "filters"))
	require.False(t, schemaHasProperty(map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{}},
	}, "filters"))
	require.True(t, schemaHasProperty(json.RawMessage(`{"properties":{"filters":{}}}`), "filters"))
	require.False(t, schemaHasProperty(json.RawMessage(`{broken`), "filters"))
	require.False(t, schemaHasProperty(nil, "filters"))
	require.False(t, schemaHasProperty("not a schema", "filters"))
}

func TestMem0WorkingDirFilter(t *testing.T) {
	f := mem0WorkingDirFilter("/repo/a")
	and, ok := f["AND"].([]any)
	require.True(t, ok)
	require.Len(t, and, 2)
	require.Equal(t, map[string]any{"agent_id": mem0AgentID}, and[0])
	require.Equal(t, map[string]any{"metadata.working_dir": "/repo/a"}, and[1])
}
