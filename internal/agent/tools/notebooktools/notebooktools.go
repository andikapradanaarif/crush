// Package notebooktools provides the notebook retrieval tools (recall
// and notebook_search) that let the model query the per-event context
// notebook.
package notebooktools

import (
	"charm.land/fantasy"
	notebooktool "github.com/charmbracelet/crush/internal/agent/tools/notebook"
	"github.com/charmbracelet/crush/internal/notebook"
)

// Build returns the notebook retrieval tools for the given service.
func Build(svc notebook.Service) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		notebooktool.NewRecallTool(svc),
		notebooktool.NewSearchTool(svc),
	}
}
