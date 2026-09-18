package agent

import (
	"slices"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestCoderPrompt_ModeAware is the issue-39 blocker contract: the
// headless prompt must never name the question tool — a model that
// hallucinates a call to a tool it doesn't have burns turns on
// tool-not-found errors.
func TestCoderPrompt_ModeAware(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := config.Init(dir, "", false)
	require.NoError(t, err)

	interactive, err := coderPrompt(
		prompt.WithWorkingDir(dir),
		prompt.WithInteractive(true),
	)
	require.NoError(t, err)
	built, err := interactive.Build(t.Context(), "prov", "model", store)
	require.NoError(t, err)
	require.Contains(t, built.Text, "question tool")
	require.Contains(t, built.Text, "Ask one focused question")

	headless, err := coderPrompt(
		prompt.WithWorkingDir(dir),
		prompt.WithInteractive(false),
	)
	require.NoError(t, err)
	builtH, err := headless.Build(t.Context(), "prov", "model", store)
	require.NoError(t, err)
	require.NotContains(t, builtH.Text, "question tool")
	require.Contains(t, builtH.Text, "cannot ask the user")
	require.Contains(t, builtH.Text, "state it in one line")
}

// TestCoderPrompt_MapHint is the same contract for the project index:
// the map hint must render only when the tool is registered —
// otherwise the model calls a tool it doesn't have.
func TestCoderPrompt_MapHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	render := func(enabled bool) string {
		store, err := config.Init(dir, "", false)
		require.NoError(t, err)
		store.Config().Options.ProjectIndex = &enabled
		p, err := coderPrompt(prompt.WithWorkingDir(dir))
		require.NoError(t, err)
		built, err := p.Build(t.Context(), "prov", "model", store)
		require.NoError(t, err)
		return built.Text
	}

	require.Contains(t, render(true), "call `map` first")
	require.NotContains(t, render(false), "call `map` first")
}

// TestCoderPrompt_NotebookToolGating is the issue-66 contract for the
// system prompt: the Context Notebook section must not advertise
// lookup tools the agent's allowed_tools exclude — a bullet naming a
// tool the model can't call is a dead pointer.
func TestCoderPrompt_NotebookToolGating(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	render := func(drop ...string) string {
		store, err := config.Init(dir, "", false)
		require.NoError(t, err)
		// Neutralize ambient global config — a developer's
		// disabled_tools, agent overrides, or notebook toggle would
		// otherwise leak into the render and break the assertions.
		pinCassetteConfig(store)
		on := true
		store.Config().Options.NotebookEnabled = &on
		store.Config().Options.DisabledTools = nil
		store.Config().Agents = nil
		store.Config().SetupAgents()
		coder := store.Config().Agents[config.AgentCoder]
		coder.AllowedTools = slices.DeleteFunc(coder.AllowedTools, func(name string) bool {
			return slices.Contains(drop, name)
		})
		store.Config().Agents[config.AgentCoder] = coder
		p, err := coderPrompt(prompt.WithWorkingDir(dir))
		require.NoError(t, err)
		built, err := p.Build(t.Context(), "prov", "model", store)
		require.NoError(t, err)
		return built.Text
	}

	full := render()
	require.Contains(t, full, "# Context Notebook")
	require.Contains(t, full, "Use the `recall` tool")
	require.Contains(t, full, "Use `notebook_search`")

	noRecall := render("recall")
	require.Contains(t, noRecall, "# Context Notebook")
	require.Contains(t, noRecall, "Use `notebook_search`")
	require.NotContains(t, noRecall, "`recall`")

	neither := render("recall", "notebook_search")
	require.Contains(t, neither, "# Context Notebook")
	require.NotContains(t, neither, "`recall`")
	require.NotContains(t, neither, "`notebook_search`")
}
