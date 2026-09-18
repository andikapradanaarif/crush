package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	notebooktool "github.com/charmbracelet/crush/internal/agent/tools/notebook"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// TestRecallHint verifies the omitted-turns breadcrumb names only the
// notebook tools the agent's live tool set actually exposes — an agent
// without recall must not be pointed at it.
func TestRecallHint(t *testing.T) {
	t.Parallel()

	agentWith := func(names ...string) *sessionAgent {
		var tools []fantasy.AgentTool
		for _, n := range names {
			tools = append(tools, &fakeTool{name: n})
		}
		return &sessionAgent{tools: csync.NewSliceFrom(tools)}
	}

	require.Equal(t,
		" — recallable via recall/notebook_search",
		agentWith("recall", "notebook_search").recallHint())
	require.Equal(t,
		" — recallable via recall",
		agentWith(notebooktool.RecallToolName).recallHint())
	require.Equal(t,
		" — recallable via notebook_search",
		agentWith(notebooktool.SearchToolName).recallHint())
	require.Equal(t, "", agentWith("view", "grep").recallHint())
	require.Equal(t, "", agentWith().recallHint())
}

// TestCoderPromptGatesRecallBullets verifies the Context Notebook system
// prompt section only advertises recall when the coder agent's resolved
// tool list contains it — a recall-less coder must not be instructed to
// call a tool that isn't in its toolset.
func TestCoderPromptGatesRecallBullets(t *testing.T) {
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true%s},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`

	build := func(t *testing.T, disabled string) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "crush.json"),
			[]byte(fmt.Sprintf(crushJSON, disabled)), 0o644))

		cfg, err := config.Init(dir, "", false)
		require.NoError(t, err)
		cfg.SetupAgents()

		p, err := coderPrompt(prompt.WithWorkingDir(dir))
		require.NoError(t, err)
		systemPrompt, err := p.Build(context.Background(), "mock", "mock-model", cfg)
		require.NoError(t, err)
		return systemPrompt.Text
	}

	withRecall := build(t, "")
	require.Contains(t, withRecall, "# Context Notebook")
	require.Contains(t, withRecall, "Use the `recall` tool")
	require.Contains(t, withRecall, "use `recall` first")
	require.Contains(t, withRecall, "Use `notebook_search` to browse")
	require.Contains(t, withRecall, "checkpoint` entry")

	withoutRecall := build(t, `, "disabled_tools": ["recall"]`)
	require.Contains(t, withoutRecall, "# Context Notebook")
	require.NotContains(t, withoutRecall, "Use the `recall` tool")
	require.NotContains(t, withoutRecall, "use `recall` first")
	require.NotContains(t, withoutRecall, "If recall doesn't have what you need")
	// notebook_search survives a recall-only disable and the checkpoint
	// bullet is tool-agnostic — entries inject regardless.
	require.Contains(t, withoutRecall, "Use `notebook_search` to browse")
	require.Contains(t, withoutRecall, "checkpoint` entry")
	require.Contains(t, withoutRecall, "Re-read files with `view`")

	// The reverse split: recall survives while its browse companion is
	// disabled — the section must not advertise notebook_search.
	withoutSearch := build(t, `, "disabled_tools": ["notebook_search"]`)
	require.Contains(t, withoutSearch, "Use the `recall` tool")
	require.NotContains(t, withoutSearch, "Use `notebook_search` to browse")

	// Neither tool: the section still renders (entries inject) but
	// advertises no recovery path at all.
	withoutBoth := build(t, `, "disabled_tools": ["recall", "notebook_search"]`)
	require.Contains(t, withoutBoth, "# Context Notebook")
	require.NotContains(t, withoutBoth, "Use the `recall` tool")
	require.NotContains(t, withoutBoth, "use `recall` first")
	require.NotContains(t, withoutBoth, "notebook_search")
	require.Contains(t, withoutBoth, "Re-read files with `view`")
	require.Contains(t, withoutBoth, "checkpoint` entry")
}
