# Crush Development Guide

## Project Overview

Crush is a terminal-based AI coding assistant built in Go by
[Charm](https://charm.land). It connects to LLMs and gives them tools to read,
write, and execute code. It supports multiple providers (Anthropic, OpenAI,
Gemini, Bedrock, Copilot, Hyper, MiniMax, Vercel, and more), integrates with
LSPs for code intelligence, and supports extensibility via MCP servers and
agent skills.

The module path is `github.com/charmbracelet/crush`.

This fork's direction and divergences from upstream are documented in
`VISION.md`, and `docs/design/`.

## Fork & Remotes

This repository is a fork of `charmbracelet/crush`. The remotes are:

- `origin` → `andikapradanaarif/crush` — **the fork**. All branches are
  pushed here and all PRs are opened against this repo's `main`.
- `upstream` → `charmbracelet/crush` — **read-only reference** remote.
  Fetch from it to stay current, but never push to it or open PRs against
  it.

The Go module path stays `github.com/charmbracelet/crush` — do not rename
it for the fork.

## Architecture

```
main.go                            CLI entry point (cobra via internal/cmd)
internal/
  app/app.go                       Top-level wiring: DB, config, agents, LSP, MCP, events
  backend/                         Transport-agnostic ops (workspaces, sessions,
                                   agents, permissions, events); consumed by
                                   server (HTTP) and ACP
  server/                          HTTP/Unix-socket server exposing the backend
  client/                          HTTP client SDK for the server
  proto/                           Wire types shared by client, server, backend
  apigen/                          OpenAPI doc generation for the server API
  workspace/                       Workspace interface for frontends (TUI, CLI);
                                   backed by a local app.App or the HTTP client
  cmd/                             CLI commands (root, run, server, eval, login,
                                   logout, models, stats, session, projects, ...)
  config/
    config.go                      Config struct, context file paths, agent definitions
    load.go                        crushrc and crush.json loading and validation
    provider.go                    Provider configuration and model resolution
  shellconfig/                     Bash-powered config format (crushrc builtins)
  agent/
    agent.go                       SessionAgent: runs LLM conversations per session
    coordinator.go                 Coordinator: manages named agents ("coder", "task")
    hooked_tool.go                 Decorator that runs PreToolUse hooks before tool execution
    prompts.go                     Loads Go-template system prompts
    prompt/                        System prompt section assembly
    templates/                     System prompt templates (coder.md.tpl, task.md.tpl, etc.)
    tools/                         All built-in tools (bash, edit, view, grep, glob,
                                   LSP tools, web fetch/search, todos, notebook, ...)
      mcp/                         MCP client integration
      notebook/                    Notebook recall/search tools
    notify/                        Turn-completion notifications
    hyper/                         Hyper provider integration
    agenttest/                     Agent test helpers
  hooks/                           Hook engine: runs user shell commands on hook events
    hooks.go                       Decision types, aggregation logic, event constants
    runner.go                      Parallel hook execution, timeout, dedup
    input.go                       Stdin payload builder, env vars, stdout parsing
                                   (Crush + Claude Code compat)
  session/session.go               Session CRUD backed by SQLite
  message/                         Message model and content types
  notebook/                        Per-event context summarization + consolidated
                                   checkpoints for sessions
  toolclass/                       Mutating-call classification vocabulary (leaf;
                                   re-exported by agent/tools)
  index/                           Persistent per-project source map (symbols, refs)
  db/                              SQLite via sqlc, with migrations
    sql/                           Raw SQL queries (consumed by sqlc)
    migrations/                    Schema migrations
  lsp/                             LSP client manager, auto-discovery, on-demand startup
  ui/                              Bubble Tea v2 TUI (see internal/ui/AGENTS.md)
  permission/                      Tool permission checking and allow-lists
  question/                        Ask-the-user service over pubsub
  skills/                          Skill file discovery and loading (incl. builtin/)
  shell/                           Bash command execution with background job support
  commands/                        Custom slash commands and MCP prompts
  projects/                        Project registry
  eval/                            Eval harness (driver, runner, coverage, gates)
  discover/                        Local LLM server discovery (Ollama, llama.cpp,
                                   LM Studio, LiteLLM, MLX)
  oauth/                           OAuth flows (callback, copilot, hyper, mcp, openai)
  login/, logout/                  CLI auth flows (TUI + non-interactive)
  update/                          Update checks against GitHub releases
  herdr/                           herdr terminal multiplexer integration
  event/                           Telemetry (PostHog)
  pubsub/                          Internal pub/sub for cross-component messaging
  filetracker/                     Tracks files touched per session
  history/                         Prompt history
eval/                              Eval corpus, experiments, and flags
docs/                              Docs: config/, design/ (design docs), hooks/
```

The remaining `internal/` packages are small utilities: `ansiext`,
`clipboard`, `csync` (concurrent data structures), `diff`, `diffdetect`,
`dns`, `env`, `filepathext`, `format`, `fsext`, `home`, `lock`, `log`,
`stringext`, `version`.

### Key Dependency Roles

- **`charm.land/fantasy`**: LLM provider abstraction layer. Handles protocol
  differences between Anthropic, OpenAI, Gemini, etc. Used in `internal/app`
  and `internal/agent`.
- **`charm.land/bubbletea/v2`**: TUI framework powering the interactive UI.
- **`charm.land/lipgloss/v2`**: Terminal styling.
- **`charm.land/glamour/v2`**: Markdown rendering in the terminal.
- **`charm.land/catwalk`**: Provider/model catalog (model metadata used by
  `internal/config` and provider resolution).
- **`github.com/charmbracelet/x/exp/golden`**: Golden-file testing for TUI
  components (`golden.RequireEqual`, `-update` flag).
- **`charm.land/x/vcr`**: HTTP cassettes for LLM provider tests (see
  Testing Without API Calls).
- **`sqlc`**: Generates Go code from SQL queries in `internal/db/sql/`.

### Key Patterns

- **Config is a Service**: accessed via `config.Service`, not global state.
- **Client/server mode**: `crush server` exposes `internal/backend`
  operations over HTTP/Unix socket (`internal/server`, `internal/client`,
  `internal/proto`). Frontends code against `internal/workspace.Workspace`,
  which wraps either a local `app.App` or the remote client.
- **Tools are self-documenting**: each tool has a `.go` implementation and a
  `.md`/`.md.tpl` description file in `internal/agent/tools/`.
- **System prompts are Go templates**: `internal/agent/templates/*.md.tpl`
  with runtime data injected.
- **Context files**: Crush reads AGENTS.md, CRUSH.md, CLAUDE.md, GEMINI.md
  (and `.local` variants) from the working directory for project-specific
  instructions.
- **Bash config format**: Crush's primary config format is `crushrc` — a
  Bash script using builtins (`provider`, `model`, `mcp`, `lsp`,
  `permissions`, `hook`, `options`) to define config. `crush.json` is still
  supported but is deprecated in favor of `crushrc` and may be removed in a
  future release. Shell config files are discovered alongside JSON configs
  and deep-merged through the same pipeline. Builtins are registered via
  `shell.RegisterBuiltin` and gated by a `ConfigBuilder` on the context —
  they are no-ops during normal bash tool execution. See
  `internal/shellconfig/`.
- **Persistence**: SQLite + sqlc. All queries live in `internal/db/sql/`,
  generated code in `internal/db/`. Migrations in `internal/db/migrations/`.
- **Pub/sub**: `internal/pubsub` for decoupled communication between agent,
  UI, and services.
- **Hooks**: User-defined shell commands in `crushrc` (or `crush.json`)
  that fire before tool execution. The engine (`internal/hooks/`) is
  independent of fantasy and agent — it takes inputs, runs commands,
  returns decisions. The `hookedTool` decorator in
  `internal/agent/hooked_tool.go` wraps tools at the coordinator level.
  Hooks run before permission checks. See `docs/hooks/README.md` for the
  user-facing protocol.
- **CGO disabled**: builds with `CGO_ENABLED=0` and
  `GOEXPERIMENT=greenteagc`.

## Build/Test/Lint Commands

- **Build**: `go build .` or `go run .`
- **Test**: `task test` or `go test ./...` (run single test:
  `go test ./internal/agent/prompt -run TestBuildSectionsPopulated`)
- **Update Golden Files**: `go test ./... -update` (regenerates `.golden`
  files when test output changes)
  - Update specific package:
    `go test ./internal/ui/diffview -update` (in this case,
    we're updating "diffview")
- **Lint**: `task lint:fix`
- **Format**: `task fmt` (`gofumpt -w .`)
- **Modernize**: `task modernize` (runs `modernize` which makes code
  simplifications)
- **Dev**: `task dev` (runs with profiling enabled)

## Code Style Guidelines

- **Imports**: Use `goimports` formatting, group stdlib, external, internal
  packages.
- **Formatting**: Use gofumpt (stricter than gofmt), enabled in
  golangci-lint.
- **Naming**: Standard Go conventions — PascalCase for exported, camelCase
  for unexported.
- **Types**: Prefer explicit types, use type aliases for clarity (e.g.,
  `type AgentName string`).
- **Error handling**: Return errors explicitly, use `fmt.Errorf` for
  wrapping.
- **Context**: Always pass `context.Context` as first parameter for
  operations.
- **Interfaces**: Define interfaces in consuming packages, keep them small
  and focused.
- **Structs**: Use struct embedding for composition, group related fields.
- **Constants**: Use typed constants with iota for enums, group in const
  blocks.
- **Testing**: Use testify's `require` package, parallel tests with
  `t.Parallel()`, `t.SetEnv()` to set environment variables. Always use
  `t.Tempdir()` when in need of a temporary directory. This directory does
  not need to be removed.
- **JSON tags**: Use snake_case for JSON field names.
- **File permissions**: Use octal notation (0o755, 0o644) for file
  permissions.
- **Log messages**: Log messages must start with a capital letter (e.g.,
  "Failed to save session" not "failed to save session").
  - This is enforced by `task lint:log` which runs as part of `task lint`.
- **Comments**: End comments in periods unless comments are at the end of the
  line.

## Testing Without API Calls

- Tests that hit LLM providers use `charm.land/x/vcr` cassettes — see
  `internal/agent/*_test.go`. Re-record all cassettes with
  `task test:record`.
- For tests that need provider configurations, build fixtures directly as
  `map[string]config.ProviderConfig` with `catwalk.Model` entries — see
  `setupMockProviders` in `internal/app/provider_test.go`. There is no
  global mock-providers switch.

## Formatting

- ALWAYS format any Go code you write.
  - First, try `gofumpt -w .`.
  - If `gofumpt` is not available, use `goimports`.
  - If `goimports` is not available, use `gofmt`.
  - You can also use `task fmt` to run `gofumpt -w .` on the entire project,
    as long as `gofumpt` is on the `PATH`.

## Comments

- Comments that live on their own lines should start with capital letters and
  end with periods. Wrap comments at 78 columns.

## Committing

- ALWAYS use semantic commits (`fix:`, `feat:`, `chore:`, `refactor:`,
  `docs:`, `sec:`, etc).
- Try to keep commits to one line, not including your attribution. Only use
  multi-line commits when additional context is truly necessary.
- NEVER force-push. To update a stacked branch onto a new base, merge the
  base branch into it (`git merge <base>`) instead of rebasing — merge
  commits are fine since PRs are squash-merged.
- Push to `origin` (the fork) only — never `upstream`.

## Pull Requests

- Always open PRs against the fork `andikapradanaarif/crush` (base
  `main`), never upstream `charmbracelet/crush`.
- `gh` defaults to the upstream repo when an `upstream` remote exists —
  always pass `--repo andikapradanaarif/crush` to `gh` commands like
  `gh pr create`.
- Push the PR branch to `origin` before creating the PR.

## Working on the TUI (UI)

Anytime you need to work on the TUI, read `internal/ui/AGENTS.md` before
starting work.

## Styling System

The styling system lives in `internal/ui/styles/` and is organized into
three layers:

- **`quickstyle.go`**: The stable base theme builder. `quickStyle(opts)`
  constructs a `Styles` struct from `quickStyleOpts` — a palette of
  design tokens (primary, secondary, fgBase, bgBase, success, error, etc.).
  `quickStyle` must be fully token-driven: never hardcode specific
  `charmtone.*` colors here (except Chroma syntax highlighting, which is
  pending tokenization). This lets any theme reuse the base without
  inheriting Charmtone-specific colors.
- **`themes.go`**: Defines concrete themes. Each theme function (e.g.
  `CharmtonePantera`) calls `quickStyle` with its palette, then applies
  theme-specific overrides as needed.
- **`styles.go`**: Defines the `Styles` struct and its documentation —
  the shape of what `quickStyle` produces.

**Adding theme-specific overrides**: When a style genuinely needs a
color that doesn't fit the token model (e.g. the bang prompt uses
Salt/Hazy/Larple), keep `quickStyle` on the closest semantic token and
override only the differing colors in the theme function:

```go
func CharmtonePantera() Styles {
	s := quickStyle(quickStyleOpts{ /* palette */ })

	// Override only the colors that differ from the token defaults.
	s.Editor.PromptBangIconFocused = s.Editor.PromptBangIconFocused.
		Foreground(charmtone.Salt).
		Background(charmtone.Hazy)

	return s
}
```

**Adding a new theme**: Add a function in `themes.go` that returns the
result of `quickStyle` with a `quickStyleOpts` palette (plus any needed
overrides), then wire it into `ThemeForProvider`.
