package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	store, err := config.Init(t.Context(), dir, "", false)
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
		store, err := config.Init(t.Context(), dir, "", false)
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
		store, err := config.Init(t.Context(), dir, "", false)
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

	// Content pins normalize whitespace — the bullets wrap mid-sentence
	// and only the rendered contract matters, not the wrap point.
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	full := render()
	require.Contains(t, full, "# Context Notebook")
	require.Contains(t, full, "Use the `recall` tool")
	require.Contains(t, full, "Use `notebook_search`")
	require.Contains(t, norm(full), "`recall` for event details, `view` for the bytes you'll modify.")
	require.NotContains(t, full, "16x")

	noRecall := render("recall")
	require.Contains(t, noRecall, "# Context Notebook")
	require.Contains(t, noRecall, "Use `notebook_search`")
	require.NotContains(t, noRecall, "`recall`")
	require.Contains(t, norm(noRecall), "`view` for the bytes you'll modify.")

	neither := render("recall", "notebook_search")
	require.Contains(t, neither, "# Context Notebook")
	require.NotContains(t, neither, "`recall`")
	require.NotContains(t, neither, "`notebook_search`")

	// The fallback branches must degrade tool-agnostically too —
	// naming `view` when it's disabled would be the same dead pointer.
	noRecallNoView := render("recall", "view")
	require.Contains(t, norm(noRecallNoView), "re-read the file for the bytes you'll modify.")
	require.NotContains(t, noRecallNoView, "use `view`")

	recallNoView := render("view")
	require.Contains(t, norm(recallNoView), "`recall` for event details, re-read the file for the bytes you'll modify.")
	require.NotContains(t, recallNoView, "`view` for the bytes")
}

// promptStore returns a config store whose options.prompts is `prompts`.
func promptStore(t *testing.T, prompts map[string]string) *config.ConfigStore {
	t.Helper()
	dir := t.TempDir()
	store, err := config.Init(t.Context(), dir, "", false)
	require.NoError(t, err)
	store.Config().Options.Prompts = prompts
	return store
}

// The map in prompts.go and the list config validates against must agree, or a
// name passes validation and then resolves to nothing.
func TestBuiltinPromptsCoverNames(t *testing.T) {
	t.Parallel()

	require.Len(t, builtinPrompts, len(config.PromptNames))
	for _, name := range config.PromptNames {
		require.Contains(t, builtinPrompts, name, "config.PromptNames lists %q but builtinPrompts has no template for it", name)
		require.NotEmpty(t, builtinPrompts[name], "built-in %q prompt is empty", name)
	}
}

func TestPromptTextUsesBuiltinByDefault(t *testing.T) {
	t.Parallel()

	store := promptStore(t, nil)
	for _, name := range config.PromptNames {
		text, err := promptText(store, name)
		require.NoError(t, err)
		require.Equal(t, string(builtinPrompts[name]), text)
	}
}

// TestPromptTextWithoutOptions covers stores built by hand without Options,
// which tests elsewhere do. They must get the built-in prompt, not a panic.
func TestPromptTextWithoutOptions(t *testing.T) {
	t.Parallel()

	store := config.NewTestStore(&config.Config{})
	text, err := promptText(store, "title")
	require.NoError(t, err)
	require.Equal(t, string(builtinPrompts["title"]), text)
}

func TestPromptTextUsesOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "coder.md.tpl")
	require.NoError(t, os.WriteFile(path, []byte("be brief"), 0o644))

	store := promptStore(t, map[string]string{"coder": path})

	text, err := promptText(store, "coder")
	require.NoError(t, err)
	require.Equal(t, "be brief", text)

	// Overriding one prompt leaves the others on their built-in.
	task, err := promptText(store, "task")
	require.NoError(t, err)
	require.Equal(t, string(taskPromptTmpl), task)
}

func TestPromptTextExpandsPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "summary.md"), []byte("sum it up"), 0o644))
	t.Setenv("CRUSH_TEST_PROMPT_DIR", dir)

	store := promptStore(t, map[string]string{"summary": "$CRUSH_TEST_PROMPT_DIR/summary.md"})

	text, err := promptText(store, "summary")
	require.NoError(t, err)
	require.Equal(t, "sum it up", text)
}

func TestPromptTextMissingFileFails(t *testing.T) {
	t.Parallel()

	store := promptStore(t, map[string]string{"coder": filepath.Join(t.TempDir(), "nope.md")})

	_, err := promptText(store, "coder")
	require.ErrorContains(t, err, "reading coder prompt")
}

func TestPromptTextUnknownNameFails(t *testing.T) {
	t.Parallel()

	_, err := promptText(promptStore(t, nil), "codr")
	require.ErrorContains(t, err, `unknown prompt "codr"`)
}

// A malformed override must fail when the prompt is loaded, not on every turn
// that builds it.
func TestLoadPromptRejectsMalformedTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "coder.md.tpl")
	require.NoError(t, os.WriteFile(path, []byte("hi {{.WorkingDir"), 0o644))

	_, err := loadPrompt(promptStore(t, map[string]string{"coder": path}), "coder")
	require.ErrorContains(t, err, "parsing coder prompt template")
}

func TestValidatePromptsRejectsUnknownName(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{
		Prompts: config.PromptPaths{"codr": "/tmp/x.md"},
	}}
	require.ErrorContains(t, cfg.ValidatePrompts(), `unknown prompt "codr"`)

	cfg.Options.Prompts = config.PromptPaths{"coder": "/tmp/x.md"}
	require.NoError(t, cfg.ValidatePrompts())
}

// A relative options.prompts path belongs to the workspace, not to whatever
// directory the process happened to start in (--cwd, server workspaces).
func TestPromptTextResolvesRelativeToWorkspace(t *testing.T) {
	t.Parallel()

	store := promptStore(t, map[string]string{"coder": ".crush/coder.md.tpl"})
	dir := filepath.Join(store.WorkingDir(), ".crush")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "coder.md.tpl"), []byte("workspace coder"), 0o644))

	text, err := promptText(store, "coder")
	require.NoError(t, err)
	require.Equal(t, "workspace coder", text)
}

func TestPromptTextRuntimeOverrideWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configured := filepath.Join(dir, "configured.md")
	flagged := filepath.Join(dir, "flagged.md")
	require.NoError(t, os.WriteFile(configured, []byte("configured"), 0o644))
	require.NoError(t, os.WriteFile(flagged, []byte("flagged"), 0o644))

	store := promptStore(t, map[string]string{"coder": configured, "task": configured})
	store.Overrides().Prompts = config.PromptPaths{"coder": flagged}

	coder, err := promptText(store, "coder")
	require.NoError(t, err)
	require.Equal(t, "flagged", coder)

	// A flag for one prompt leaves the config for the others alone.
	task, err := promptText(store, "task")
	require.NoError(t, err)
	require.Equal(t, "configured", task)
}

// Summary and title are only used mid-session, so their files must be read
// up front or a typo surfaces the first time a session compacts.
func TestLoadPromptTextsCatchesMissingLazyPrompt(t *testing.T) {
	t.Parallel()

	store := promptStore(t, map[string]string{"summary": filepath.Join(t.TempDir(), "nope.md")})
	_, err := loadPromptTexts(store)
	require.ErrorContains(t, err, "reading summary prompt")
}

func TestLoadPromptTextsCatchesMalformedTemplate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "plan.md.tpl")
	require.NoError(t, os.WriteFile(path, []byte("{{.WorkingDir"), 0o644))

	store := promptStore(t, map[string]string{"plan": path})
	_, err := loadPromptTexts(store)
	require.ErrorContains(t, err, "parsing plan prompt template")
}

// Title and summary are sent verbatim, so braces in them are just text.
func TestLoadPromptTextsAllowsBracesInPlainPrompts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "title.md")
	require.NoError(t, os.WriteFile(path, []byte("name it {{ like this"), 0o644))

	store := promptStore(t, map[string]string{"title": path})
	_, err := loadPromptTexts(store)
	require.NoError(t, err)
}

func TestLoadPromptTextsWithoutOverrides(t *testing.T) {
	t.Parallel()

	texts, err := loadPromptTexts(promptStore(t, nil))
	require.NoError(t, err)
	for _, name := range config.PromptNames {
		require.Equal(t, string(builtinPrompts[name]), texts[name])
	}
}

// A replacement is read once at startup, so moving or deleting the file
// mid-session must not break compaction or titles.
func TestSessionAgentKeepsPromptsAfterFileIsGone(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "summary.md")
	require.NoError(t, os.WriteFile(path, []byte("sum it up"), 0o644))

	texts, err := loadPromptTexts(promptStore(t, map[string]string{"summary": path}))
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	a := NewSessionAgent(SessionAgentOptions{Prompts: texts}).(*sessionAgent)
	require.Equal(t, "sum it up", promptOrBuiltin(a.prompts, "summary"))
	require.Equal(t, string(titlePromptTmpl), promptOrBuiltin(a.prompts, "title"))

	bare := NewSessionAgent(SessionAgentOptions{}).(*sessionAgent)
	require.Equal(t, string(summaryPromptTmpl), promptOrBuiltin(bare.prompts, "summary"))
}

// coderPrompt and planPrompt build the built-in templates directly —
// test shims for callers that predate options.prompts overrides.
func coderPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	return prompt.NewPrompt("coder", string(coderPromptTmpl), opts...)
}

func planPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	return prompt.NewPrompt("plan", string(planPromptTmpl), opts...)
}
