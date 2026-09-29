package notebook

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// SyncEntries must not carry credential material across the trust
// boundary — an entry summarizing a .env read keeps its context but
// loses the secret.
func TestSyncEntriesRedactsSecrets(t *testing.T) {
	cfg := mem0TestStore(t, t.TempDir())
	var sent []string
	stubRunMCPTool(t, func(_ context.Context, _ *config.ConfigStore, _, _ string, input string) (mcp.ToolResult, error) {
		sent = append(sent, input)
		return mcp.ToolResult{Type: "text", Content: `{"ok":true}`}, nil
	})

	NewMem0Sync(cfg, "mem0").SyncEntries(context.Background(), []Entry{{
		SessionID:     "s1",
		EventType:     EventFileEdit,
		Title:         "wrote .env",
		EntryTextFull: "created .env with API_KEY=supersecretvalue123",
	}})
	require.Len(t, sent, 1)
	require.NotContains(t, sent[0], "supersecretvalue123")
	require.Contains(t, sent[0], "[REDACTED]")
	require.Contains(t, sent[0], "created .env with")
}
