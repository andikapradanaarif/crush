package notebook

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRedactSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		// wantGone is the sensitive material that must not survive;
		// wantKept is innocent context that must.
		wantGone string
		wantKept string
	}{
		{"env assignment", "DATABASE_URL=postgres://u:p@h/db\nAPI_KEY=abc123secret", "abc123secret", "DATABASE_URL"},
		{"json field", `{"password": "hunter2", "user": "me"}`, "hunter2", `"user": "me"`},
		{"bearer header", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", "eyJhbGciOiJIUzI1NiJ9", "Authorization"},
		{"openai key", "set OPENAI key to sk-proj-abcdefghijklmnop12345", "sk-proj-abcdefghijklmnop", "OPENAI"},
		{"github pat", "token ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6", "ghp_A1b2C3d4", "token"},
		{"github fine-grained", "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "github_pat_11ABCDEFG0", ""},
		{"aws access key", "aws key AKIAIOSFODNN7EXAMPLE in config", "AKIAIOSFODNN7EXAMPLE", "aws key"},
		{"slack token", "xoxb-123456789012-abcdefghijkl leaked", "xoxb-123456789012", "leaked"},
		{"private key block", "cert:\n-----BEGIN RSA PRIVATE KEY-----\nMIIBogIBAAJB\n-----END RSA PRIVATE KEY-----\ndone", "MIIBogIBAAJB", "done"},
		{"no secrets", "edited internal/config.go and ran go test", "", "edited internal/config.go"},
		{"hash survives", "commit 5de778d6d9784648b63db15be290fc359963b513 merged", "", "5de778d6d9784648b63db15be290fc359963b513"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := redactSecrets(tt.in)
			if tt.wantGone != "" {
				require.NotContains(t, out, tt.wantGone)
				require.Contains(t, out, redactedPlaceholder)
			}
			if tt.wantKept != "" {
				require.Contains(t, out, tt.wantKept)
			}
			if tt.wantGone == "" {
				require.Equal(t, tt.in, out)
			}
		})
	}
}

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
	require.Contains(t, sent[0], redactedPlaceholder)
	require.Contains(t, sent[0], "created .env with")
}
