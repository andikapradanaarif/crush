package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// reasoningTestConfig builds the minimal config the reasoning-effort
// palette gate reads: one provider carrying a reasoning-capable model
// and a plain one, the coder agent bound to the given model slot.
func reasoningTestConfig(slot config.SelectedModelType, sel config.SelectedModel) *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("test", config.ProviderConfig{
		ID:   "test",
		Name: "Test",
		Models: []catwalk.Model{
			{ID: "reasoning-model", CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}},
			{ID: "plain-model"},
		},
	})
	return &config.Config{
		Providers: providers,
		Agents: map[string]config.Agent{
			string(config.AgentCoder): {Model: slot},
		},
		Models: map[config.SelectedModelType]config.SelectedModel{
			slot: sel,
		},
	}
}

func findReasoningEffortCommand(commands []*CommandItem) *CommandItem {
	for _, cmd := range commands {
		if cmd.id == "select_reasoning_effort" {
			return cmd
		}
	}
	return nil
}

func newReasoningTestCommands(t *testing.T, cfg *config.Config, sessionModel *config.SelectedModel) *Commands {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s, Workspace: &stubWorkspace{cfg: cfg}}
	cmds, err := NewCommands(com, "sess", true, false, false, nil, nil, sessionModel)
	require.NoError(t, err)
	return cmds
}

// TestReasoningEffortCommandGatesOnSessionOverride pins the
// session-aware gate: the palette entry follows the session's
// effective model, not the configured slot — so a session pinned to
// a reasoning model gets the command on a plain shared model, and a
// session pinned to a plain model loses it on a reasoning shared
// model.
func TestReasoningEffortCommandGatesOnSessionOverride(t *testing.T) {
	t.Parallel()

	t.Run("no override shows command on reasoning model", func(t *testing.T) {
		t.Parallel()
		cfg := reasoningTestConfig(config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: "test", Model: "reasoning-model",
		})
		cmds := newReasoningTestCommands(t, cfg, nil)
		require.NotNil(t, findReasoningEffortCommand(cmds.defaultCommands()))
	})

	t.Run("override to reasoning model shows command on plain shared model", func(t *testing.T) {
		t.Parallel()
		cfg := reasoningTestConfig(config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: "test", Model: "plain-model",
		})
		override := &config.SelectedModel{Provider: "test", Model: "reasoning-model"}
		cmds := newReasoningTestCommands(t, cfg, override)
		require.NotNil(t, findReasoningEffortCommand(cmds.defaultCommands()),
			"session pinned to a reasoning model must offer the effort dialog")
	})

	t.Run("override to plain model hides command on reasoning shared model", func(t *testing.T) {
		t.Parallel()
		cfg := reasoningTestConfig(config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: "test", Model: "reasoning-model",
		})
		override := &config.SelectedModel{Provider: "test", Model: "plain-model"}
		cmds := newReasoningTestCommands(t, cfg, override)
		require.Nil(t, findReasoningEffortCommand(cmds.defaultCommands()),
			"session pinned to a non-reasoning model must not offer the effort dialog")
	})
}
