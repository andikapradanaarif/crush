package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt string `json:"prompt" description:"The task for the agent to perform"`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	// The task sub-agent is built lazily on first use: its configured
	// model slot may reference a provider that failed to configure, and
	// a stale secondary selection must not brick the parent agent's
	// readiness. A build failure surfaces at call time as a tool error
	// naming the provider/model, and rebuilds retry on later calls so a
	// config reload can fix it without restarting.
	var (
		buildMu  sync.Mutex
		subAgent SessionAgent
	)
	build := func() error {
		built, err := c.buildAgent(ctx, prompt, agentCfg, true)
		if err != nil {
			return fmt.Errorf("task agent unavailable: %w", err)
		}
		subAgent = built
		return nil
	}
	ensureAgent := func() (SessionAgent, error) {
		buildMu.Lock()
		defer buildMu.Unlock()
		if subAgent != nil {
			return subAgent, nil
		}
		if err := build(); err != nil {
			return nil, err
		}
		return subAgent, nil
	}

	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			subAgent, err := ensureAgent()
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:          subAgent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   "New Agent Session",
			})
		},
	), nil
}
