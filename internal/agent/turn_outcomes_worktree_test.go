package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/charmbracelet/crush/internal/permission/testutil"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// Exercise a real mutation and its persisted result through extraction,
// recording, and settlement. A sibling checkout is captureable only when
// the session is rooted there, even though both share a Git common dir.
func TestTurnOutcomeCaptureInLinkedWorktree(t *testing.T) {
	t.Parallel()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	repo, linked := filepath.Join(parent, "repo"), filepath.Join(parent, "task")
	require.NoError(t, os.Mkdir(repo, 0o755))
	hooks := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		prefix := []string{
			"-c", "user.name=Crush Test", "-c", "user.email=test@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + hooks, "-C", repo,
		}
		cmd := exec.CommandContext(t.Context(), "git", append(prefix, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	git("init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* text eol=lf\n"), 0o644))
	for _, name := range []string{"parent.txt", "worktree.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte("head\nbefore\nfoot\n"), 0o644))
	}
	git("add", ".gitattributes", "parent.txt", "worktree.txt")
	git("commit", "--quiet", "-m", "test: initial capture fixture")
	git("worktree", "add", "--quiet", "--detach", linked)
	require.Equal(t, cmdlog.ProjectKeyForDir(repo), cmdlog.ProjectKeyForDir(linked))

	for _, tc := range []struct {
		name  string
		root  string
		file  string
		state string
	}{
		{"parent session excludes sibling", repo, "parent.txt", "out_of_workspace"},
		{"worktree session captures edit", linked, "worktree.txt", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			path := filepath.Join(linked, tc.file)
			conn, err := db.Connect(ctx, filepath.Join(tc.root, ".crush"))
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			q := db.New(conn)
			sessions := session.NewService(q, conn)
			messages := message.NewService(q)
			sess, err := sessions.Create(ctx, "Capture in a worktree")
			require.NoError(t, err)
			user, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{
				Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Update the note"}},
			})
			require.NoError(t, err)
			tracker := filetracker.NewService(q, tc.root)
			tracker.RecordRead(ctx, sess.ID, path)
			tool := tools.NewEditTool(&testutil.MockPermissionService{}, history.NewService(q, conn), tracker, tc.root)
			input, err := json.Marshal(tools.EditParams{FilePath: path, OldString: "before", NewString: "after"})
			require.NoError(t, err)
			call := fantasy.ToolCall{ID: "edit-note", Name: tools.EditToolName, Input: string(input)}
			response, err := tool.Run(context.WithValue(ctx, tools.SessionIDContextKey, sess.ID), call)
			require.NoError(t, err)
			require.False(t, response.IsError, "%s", response.Content)
			_, err = messages.Create(ctx, sess.ID, message.CreateMessageParams{
				Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
					ID: call.ID, Name: call.Name, Input: call.Input, Finished: true,
				}},
			})
			require.NoError(t, err)
			_, err = messages.Create(ctx, sess.ID, message.CreateMessageParams{
				Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{
					ToolCallID: call.ID, Name: call.Name, Content: response.Content, Metadata: response.Metadata,
				}},
			})
			require.NoError(t, err)
			transcript, err := messages.List(ctx, sess.ID)
			require.NoError(t, err)
			clock := time.Now()
			store := cmdlog.NewService(q, tc.root, params.DefaultMemory(), cmdlog.WithClock(func() time.Time { return clock }))
			deps := turnOutcomeDeps{workdir: tc.root, messages: messages, sessions: sessions, cmdlog: store}
			outcome := extractTurnOutcome(ctx, deps, sess.ID, transcript, msgIdx(transcript, user.ID))
			require.Len(t, outcome.EditedPaths, 1)
			require.NoError(t, store.RecordTurnOutcome(ctx, outcome))

			type cell struct {
				State   string            `json:"state"`
				Edits   []cmdlog.HunkEdit `json:"edits"`
				Spans   []cmdlog.HunkSpan `json:"spans"`
				Windows []struct {
					Text string `json:"text"`
				} `json:"windows"`
			}
			var addedJSON string
			require.NoError(t, conn.QueryRowContext(ctx,
				"SELECT hunk_added FROM turn_outcomes WHERE turn_id = ?", user.ID).Scan(&addedJSON))
			var added map[string]cell
			require.NoError(t, json.Unmarshal([]byte(addedJSON), &added))
			captured := added[outcome.EditedPaths[0]]
			require.Equal(t, tc.state, captured.State)
			if tc.state == "ok" {
				require.Equal(t, []string{tc.file}, outcome.EditedPaths)
				require.Equal(t, []cmdlog.HunkEdit{{Old: "before", New: "after"}}, captured.Edits)
				require.Equal(t, []cmdlog.HunkSpan{{Start: 1, End: 2, Before: "head", After: "foot"}}, captured.Spans)
			} else {
				require.Empty(t, captured.Edits)
				require.Empty(t, captured.Spans)
			}

			// Advance the store's clock; keep production settlement rules.
			clock = clock.Add(25 * time.Hour)
			require.NoError(t, store.SettleTurnOutcomes(ctx, 50))
			var afterJSON string
			var settledAt int64
			require.NoError(t, conn.QueryRowContext(ctx,
				"SELECT hunk_after, settled_at FROM turn_outcomes WHERE turn_id = ?", user.ID).Scan(&afterJSON, &settledAt))
			require.Positive(t, settledAt)
			var after map[string]cell
			require.NoError(t, json.Unmarshal([]byte(afterJSON), &after))
			settled := after[outcome.EditedPaths[0]]
			require.Equal(t, tc.state, settled.State)
			if tc.state == "ok" {
				require.NotEmpty(t, settled.Windows)
				require.Contains(t, settled.Windows[0].Text, "after")
			} else {
				require.Empty(t, settled.Windows)
			}
		})
	}
}
