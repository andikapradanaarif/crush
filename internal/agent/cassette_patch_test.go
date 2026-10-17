package agent

// Throwaway test that rewrites the TestCoderAgent VCR cassettes to
// match the prompt changes from the prompt-optimization work: the new
// coder system prompt, the trimmed bash tool description, and the
// removed synthetic system_reminder message.
//
// Run once with:
//
//	CRUSH_PATCH_CASSETTES=1 go test ./internal/agent/ -run TestPatchCoderCassettes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v4"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
)

func TestPatchCoderCassettes(t *testing.T) {
	if os.Getenv("CRUSH_PATCH_CASSETTES") != "1" {
		t.Skip("set CRUSH_PATCH_CASSETTES=1 to rewrite cassettes")
	}

	dir := filepath.Join("testdata", "TestCoderAgent", "deepseek-v4")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	fixedTime := func() time.Time {
		ts, _ := time.Parse("1/2/2006", "1/1/2025")
		return ts
	}

	for _, file := range files {
		base := strings.TrimSuffix(filepath.Base(file), ".yaml")
		workingDir := filepath.Join("/tmp/crush-test/TestCoderAgent/deepseek-v4", base)
		require.NoError(t, os.MkdirAll(workingDir, 0o755))

		// Render the new system prompt exactly as coderAgent does.
		p, err := coderPrompt(
			prompt.WithTimeFunc(fixedTime),
			prompt.WithPlatform("linux"),
			prompt.WithWorkingDir(filepath.ToSlash(workingDir)),
		)
		require.NoError(t, err)

		cfg, err := config.Init(workingDir, "", false)
		require.NoError(t, err)
		pinCassetteConfig(cfg)

		built, err := p.Build(t.Context(), "hyper", "deepseek-v4-pro-0813", cfg)
		require.NoError(t, err)
		newPrompt := built.Text

		// Render the new bash description exactly as the test tool list does.
		perms := permission.NewPermissionService(workingDir, true, []string{})
		bashTool := tools.NewBashTool(nil, perms, workingDir, workingDir, cfg.Config().Options.Attribution, "")
		newBashDesc := bashTool.Info().Description

		// Patch every interaction's request body.
		c, err := cassette.Load(strings.TrimSuffix(file, ".yaml"))
		require.NoError(t, err)
		for _, in := range c.Interactions {
			if in.Request.Body == "" {
				continue // Non-LLM requests (e.g. file downloads).
			}
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(in.Request.Body), &body))

			msgs, ok := body["messages"].([]any)
			if !ok {
				continue // Non-LLM requests (e.g. sourcegraph API).
			}

			// messages[0] is the system prompt.
			first, ok := msgs[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "system", first["role"])
			first["content"] = newPrompt

			// Drop the synthetic system_reminder user message.
			filtered := msgs[:0]
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if content, _ := mm["content"].(string); mm["role"] == "user" && strings.HasPrefix(content, "<system_reminder>") {
						continue
					}
				}
				filtered = append(filtered, m)
			}
			body["messages"] = filtered

			// Update the bash tool description.
			if tl, ok := body["tools"].([]any); ok {
				for _, tool := range tl {
					tm, ok := tool.(map[string]any)
					if !ok {
						continue
					}
					fn, ok := tm["function"].(map[string]any)
					if !ok || fn["name"] != "bash" {
						continue
					}
					fn["description"] = newBashDesc
				}
			}

			patched, err := json.Marshal(body)
			require.NoError(t, err)
			in.Request.Body = string(patched)
			in.Request.ContentLength = int64(len(patched))
		}
		out, err := yaml.Marshal(c)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, out, 0o644))
		t.Logf("patched %s", file)
	}
}

// TestPatchCoderCassetteEditResults rewrites the tool-result text recorded
// for edit and multiedit calls so cassettes match the post-edit region that
// those tools now append to their success responses. It recreates the test
// fixture, replays each recorded mutating call through the real tool
// implementations in request order, and substitutes the new result text
// wherever that call's result appears in subsequent request bodies.
//
// Run once with:
//
//	CRUSH_PATCH_CASSETTES=1 go test ./internal/agent/ -run TestPatchCoderCassetteEditResults
func TestPatchCoderCassetteEditResults(t *testing.T) {
	if os.Getenv("CRUSH_PATCH_CASSETTES") != "1" {
		t.Skip("set CRUSH_PATCH_CASSETTES=1 to rewrite cassettes")
	}

	dir := filepath.Join("testdata", "TestCoderAgent", "deepseek-v4")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		base := strings.TrimSuffix(filepath.Base(file), ".yaml")
		workingDir := filepath.Join("/tmp/crush-test/TestCoderAgent/deepseek-v4", base)
		require.NoError(t, os.MkdirAll(workingDir, 0o755))
		createSimpleGoProject(t, workingDir)

		conn, err := db.Connect(t.Context(), t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })

		q := db.New(conn)
		sess, err := session.NewService(q, conn).Create(t.Context(), "patch")
		require.NoError(t, err)
		hist := history.NewService(q, conn)
		tracker := filetracker.NewService(q)
		perms := permission.NewPermissionService(workingDir, true, []string{})

		editTool := tools.NewEditTool(perms, hist, tracker, workingDir)
		multiTool := tools.NewMultiEditTool(perms, hist, tracker, workingDir)

		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sess.ID)
		ctx = context.WithValue(ctx, tools.MessageIDContextKey, "patch-message")

		c, err := cassette.Load(strings.TrimSuffix(file, ".yaml"))
		require.NoError(t, err)

		// Replay recorded mutating calls in request order, capturing the
		// result text the current tools emit.
		newResults := map[string]string{}
		for _, in := range c.Interactions {
			msgs := requestMessages(t, in)
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] != "assistant" {
					continue
				}
				tcs, _ := mm["tool_calls"].([]any)
				for _, tc := range tcs {
					tcm, _ := tc.(map[string]any)
					fn, _ := tcm["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if name != tools.EditToolName && name != tools.MultiEditToolName {
						continue
					}
					id, _ := tcm["id"].(string)
					if _, done := newResults[id]; done {
						continue
					}
					args, _ := fn["arguments"].(string)
					var parsed struct {
						FilePath string `json:"file_path"`
					}
					require.NoError(t, json.Unmarshal([]byte(args), &parsed))
					tracker.RecordRead(ctx, sess.ID, filepathext.SmartJoin(workingDir, parsed.FilePath))

					tool := editTool
					if name == tools.MultiEditToolName {
						tool = multiTool
					}
					resp, err := tool.Run(ctx, fantasy.ToolCall{ID: id, Name: name, Input: args})
					require.NoError(t, err)
					require.False(t, resp.IsError, "replayed %s call %s failed: %s", name, id, resp.Content)
					newResults[id] = resp.Content
				}
			}
		}
		if len(newResults) == 0 {
			continue
		}

		// Substitute the new result text into every tool message carrying
		// one of those calls.
		for _, in := range c.Interactions {
			var body map[string]any
			if in.Request.Body == "" {
				continue
			}
			require.NoError(t, json.Unmarshal([]byte(in.Request.Body), &body))
			msgs, ok := body["messages"].([]any)
			if !ok {
				continue
			}
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] != "tool" {
					continue
				}
				content, ok := mm["content"].(string)
				if !ok {
					continue
				}
				if newText, ok := newResults[mm["tool_call_id"].(string)]; ok && content != newText {
					mm["content"] = newText
				}
			}
			patched, err := json.Marshal(body)
			require.NoError(t, err)
			in.Request.Body = string(patched)
			in.Request.ContentLength = int64(len(patched))
		}

		out, err := yaml.Marshal(c)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, out, 0o644))
		t.Logf("patched %s", file)
	}
}

// requestMessages decodes the messages array from an interaction's request
// body, returning nil for non-LLM requests.
func requestMessages(t *testing.T, in *cassette.Interaction) []any {
	t.Helper()
	if in.Request.Body == "" {
		return nil
	}
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(in.Request.Body), &body))
	msgs, _ := body["messages"].([]any)
	return msgs
}
