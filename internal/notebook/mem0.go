package notebook

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
)

const (
	// mem0AgentID namespaces all crush-written memories inside the
	// mem0 store. Partitioning by project happens on the
	// working_dir metadata key, not the agent id.
	mem0AgentID = "crush"
	// mem0WorkingDirKey is the metadata key carrying the normalized
	// working directory path. It partitions memories per project so
	// a cross-session search in one repo can never surface another
	// repo's memories.
	mem0WorkingDirKey = "working_dir"
	// TagHydrated marks notebook entries seeded from mem0 during
	// session hydration. They must never re-sync — re-syncing would
	// duplicate the memory they came from, once per hydrated session.
	TagHydrated = "hydrated"
)

const (
	// mem0SearchTopK is the number of memories returned to the model.
	mem0SearchTopK = 10
	// mem0SearchFetchK is the over-fetch size used when the server's
	// search_memories cannot apply the working_dir filter itself.
	// Post-filtering a semantic top-10 would produce false negatives
	// — same-project memories that exist but didn't rank inside a
	// global top-10 — so the partition filter runs over a wide fetch
	// and the survivors are ranked and capped client-side.
	mem0SearchFetchK = 100
	// mem0SearchMaxTokens is the maximum token count for mem0 search
	// results returned to the model. Results beyond this are
	// truncated to avoid prompt inflation.
	mem0SearchMaxTokens = 4000
)

// runMCPTool delegates to mcp.RunTool. It is a package-level var so
// tests can stub the MCP round-trip.
var runMCPTool = mcp.RunTool

// Mem0Sync provides cross-session memory sync via an MCP server
// (typically mem0). It is optional and only active when
// NotebookSyncMem0 is enabled in config.
type Mem0Sync struct {
	cfg        *config.ConfigStore
	serverName string
	sessionID  string
}

// NewMem0Sync creates a mem0 sync helper for the given session.
func NewMem0Sync(cfg *config.ConfigStore, serverName, sessionID string) *Mem0Sync {
	return &Mem0Sync{
		cfg:        cfg,
		serverName: serverName,
		sessionID:  sessionID,
	}
}

// SyncEntries syncs notebook entries to mem0 by calling the MCP
// server's add_memory tool. Each entry is stored with its tags as
// metadata so cross-session search can filter by tag. Memories are
// partitioned by the normalized working_dir metadata key so a search
// in one project never returns another project's entries.
func (m *Mem0Sync) SyncEntries(ctx context.Context, entries []Entry) {
	if m == nil || m.cfg == nil || m.serverName == "" || len(entries) == 0 {
		return
	}
	raw := m.cfg.WorkingDir()
	if raw == "" {
		// Without a partition key the memory would be unreachable by
		// every future filtered search — writing it is dead data.
		slog.Warn("Mem0 sync skipped: working directory is unknown", "server", m.serverName)
		return
	}
	workDir := normalizeWorkingDir(raw)
	for _, entry := range entries {
		if slices.Contains(entry.Tags, TagHydrated) {
			// Seeds hydrated from mem0 carry their origin session as a
			// tag; re-syncing them would compound one copy per session.
			continue
		}
		text := entry.EntryTextFull
		if text == "" {
			text = entry.EntryText
		}
		metadata := map[string]any{
			"session_id":        entry.SessionID,
			"origin_session_id": entry.SessionID,
			"working_dir":       workDir,
			"turn_number":       entry.TurnNumber,
			"event_number":      entry.EventNumber,
			"event_type":        entry.EventType,
			"tags":              entry.Tags,
			"compression":       entry.CompressionLevel,
		}
		args := map[string]any{
			"text":     fmt.Sprintf("## %s\n%s", entry.Title, text),
			"agent_id": mem0AgentID,
			"metadata": metadata,
			"infer":    false,
		}
		input, _ := json.Marshal(args)
		_, err := runMCPTool(ctx, m.cfg, m.serverName, "add_memory", string(input))
		if err != nil {
			slog.Warn("Failed to sync entry to mem0",
				"error", err,
				"server", m.serverName,
				"turn", entry.TurnNumber,
			)
		}
	}
}

// SearchMem0 searches cross-session memories via the MCP server's
// search_memories tool, restricted to memories written from the
// current working directory. When the tool's schema accepts a
// filters argument the partition is applied server-side; otherwise
// a wide fetch is partitioned and ranked client-side. Results that
// can neither be filtered server-side nor verified against
// working_dir metadata are dropped — an empty result beats a
// wrong-project one. The result text is truncated to
// mem0SearchMaxTokens to avoid prompt inflation.
func SearchMem0(ctx context.Context, cfg *config.ConfigStore, serverName, query string) (string, error) {
	if cfg == nil || serverName == "" || strings.TrimSpace(query) == "" {
		return "", nil
	}
	raw := cfg.WorkingDir()
	if raw == "" {
		slog.Warn("Mem0 search skipped: working directory is unknown", "server", serverName)
		return "", nil
	}
	workDir := normalizeWorkingDir(raw)
	args := map[string]any{
		"query":    query,
		"agent_id": mem0AgentID,
		"top_k":    mem0SearchFetchK,
	}
	serverFiltered := mem0SearchFilterCapable(serverName)
	if serverFiltered {
		args["top_k"] = mem0SearchTopK
		args["filters"] = mem0WorkingDirFilter(workDir)
	}
	input, _ := json.Marshal(args)
	result, err := runMCPTool(ctx, cfg, serverName, "search_memories", string(input))
	if err != nil {
		return "", fmt.Errorf("mem0 search failed: %w", err)
	}
	filtered, ok := filterMem0Results(result.Content, workDir, serverName)
	if !ok {
		if serverFiltered {
			// The server enforced the partition; the response simply
			// isn't per-memory JSON we can re-check.
			return truncateTextToTokens(result.Content, mem0SearchMaxTokens), nil
		}
		slog.Warn("Mem0 search dropped: response is not parseable and no server-side filter was applied",
			"server", serverName)
		return "", nil
	}
	return truncateTextToTokens(filtered, mem0SearchMaxTokens), nil
}

// normalizeWorkingDir canonicalizes a working directory for use as
// the mem0 partition key: filepath.Abs anchors it, EvalSymlinks
// collapses symlinked spellings (macOS /var → /private/var), and on
// case-insensitive filesystems the key is case-folded — the same rule
// as the agent's normalizedPath — so symlinked or case-variant
// spellings share one partition instead of fragmenting it.
func normalizeWorkingDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs
}

// mem0WorkingDirFilter builds a mem0 filter expression matching
// memories written by crush from the given working directory.
func mem0WorkingDirFilter(workDir string) map[string]any {
	return map[string]any{
		"AND": []any{
			map[string]any{"agent_id": mem0AgentID},
			map[string]any{"metadata." + mem0WorkingDirKey: workDir},
		},
	}
}

// mem0SearchFilterCapable reports whether the server's registered
// search_memories tool declares a filters argument in its input
// schema. The registry is empty until the server connects — in that
// case the client-side over-fetch and filter still partition
// results. A package-level var so tests can force either path.
var mem0SearchFilterCapable = func(serverName string) bool {
	for name, tools := range mcp.Tools() {
		if name != serverName {
			continue
		}
		for _, tool := range tools {
			if tool != nil && tool.Name == "search_memories" {
				return schemaHasProperty(tool.InputSchema, "filters")
			}
		}
	}
	return false
}

// schemaHasProperty reports whether a JSON Schema object declares
// prop in its properties. The SDK hands the client the server's
// schema as a map[string]any, but tests and raw payloads may carry
// it as json.RawMessage — both are handled.
func schemaHasProperty(schema any, prop string) bool {
	var m map[string]any
	switch s := schema.(type) {
	case map[string]any:
		m = s
	case json.RawMessage:
		if err := json.Unmarshal(s, &m); err != nil {
			return false
		}
	default:
		return false
	}
	props, _ := m["properties"].(map[string]any)
	_, ok := props[prop]
	return ok
}

// filterMem0Results parses a search_memories response and keeps only
// memories whose metadata.working_dir equals workDir; anything else —
// foreign partitions, missing keys, malformed metadata — is excluded
// and logged. Survivors are ranked by relevance score and capped at
// mem0SearchTopK. The boolean reports whether the payload parsed into
// per-memory items at all: false means the caller cannot prove the
// results are same-project.
func filterMem0Results(content, workDir, serverName string) (string, bool) {
	var payload any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", false
	}
	items, ok := mem0ResultItems(payload)
	if !ok {
		return "", false
	}
	var kept []map[string]any
	dropped := 0
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			dropped++
			continue
		}
		wd, ok := mem0ItemWorkingDir(m)
		if !ok || wd != workDir {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	if dropped > 0 {
		slog.Warn("Excluded mem0 memories with missing, malformed, or foreign working_dir metadata",
			"server", serverName,
			"excluded", dropped,
			"kept", len(kept),
		)
	}
	// The server ranked by semantic relevance across every project;
	// after partitioning, re-rank the survivors and cap at the
	// model-facing top_k.
	slices.SortStableFunc(kept, func(a, b map[string]any) int {
		return cmp.Compare(mem0Score(b), mem0Score(a))
	})
	if len(kept) > mem0SearchTopK {
		kept = kept[:mem0SearchTopK]
	}
	if len(kept) == 0 {
		return "", true
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// mem0ResultItems extracts the list of memory items from a parsed
// search_memories payload. mem0 servers variously return a bare JSON
// array or an object wrapping one under results/memories/data/items;
// a lone object carrying metadata is treated as a single result.
func mem0ResultItems(payload any) ([]any, bool) {
	switch p := payload.(type) {
	case []any:
		return p, true
	case map[string]any:
		for _, key := range []string{"results", "memories", "data", "items"} {
			if items, ok := p[key].([]any); ok {
				return items, true
			}
		}
		if _, ok := p["metadata"]; ok {
			return []any{p}, true
		}
	}
	return nil, false
}

// mem0ItemWorkingDir returns the metadata.working_dir of one memory
// item, reporting false when the metadata is absent or the key is
// missing, empty, or not a string.
func mem0ItemWorkingDir(item map[string]any) (string, bool) {
	meta, ok := item["metadata"].(map[string]any)
	if !ok {
		return "", false
	}
	wd, ok := meta[mem0WorkingDirKey].(string)
	return wd, ok && wd != ""
}

// mem0Score extracts a relevance score from a memory item for
// client-side ranking; absent or non-numeric scores rank last.
func mem0Score(item map[string]any) float64 {
	for _, key := range []string{"score", "similarity", "relevance"} {
		if v, ok := item[key].(float64); ok {
			return v
		}
	}
	return 0
}

// truncateTextToTokens truncates text to approximately maxTokens by
// using a rough 4-chars-per-token estimate.
func truncateTextToTokens(text string, maxTokens int) string {
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	return text[:maxChars] + "\n\n[Results truncated to stay within token budget]"
}
