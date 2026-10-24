package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// A stale secondary-agent model pin (e.g. agents.task -> models.task ->
// a provider that failed to configure) must not brick the primary
// agent's readiness: the agent tool builds lazily and reports the dead
// provider at call time.
func TestAgentToolLazyBuildWithMissingProvider(t *testing.T) {
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"},
             "task": {"provider": "ghost", "model": "ghost-model"}},
  "agents": {"task": {"model": "task"}}
}`
	coord := newSummaryTestCoordinator(t, crushJSON)

	// Tool construction must not resolve the task agent's provider.
	tool, err := coord.agentTool(context.Background())
	require.NoError(t, err)
	require.Equal(t, AgentToolName, tool.Info().Name)

	// The whole tool list for the coder agent must still build.
	agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
	built, err := coord.buildTools(context.Background(), agentCfg, false)
	require.NoError(t, err)
	var names []string
	for _, bt := range built {
		names = append(names, bt.Info().Name)
	}
	require.Contains(t, names, AgentToolName)

	// Calling the tool surfaces a tool error naming the dead provider.
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "s1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "m1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  AgentToolName,
		Input: `{"prompt":"hi"}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, `"ghost"`)
}

// buildModel names the failing slot instead of blindly claiming "large"
// when an agent pins a custom model slot.
func TestBuildModelNamesNonLargeSlot(t *testing.T) {
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"},
             "task": {"provider": "ghost", "model": "ghost-model"}},
  "agents": {"task": {"model": "task"}}
}`
	coord := newSummaryTestCoordinator(t, crushJSON)

	taskCfg := coord.cfg.Config().Agents[config.AgentTask]
	_, err := coord.buildModel(context.Background(), taskCfg.Model, true)
	require.ErrorIs(t, err, errModelProviderNotConfigured)
	require.Contains(t, err.Error(), `"ghost"`)
	require.Contains(t, err.Error(), `task`)
}
