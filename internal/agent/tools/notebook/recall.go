package notebook

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/notebook"
)

// RecallToolName is the name of the recall tool.
const RecallToolName = "recall"

//go:embed recall.md
var recallDescription []byte

// RecallParams holds the parameters for the recall tool.
type RecallParams struct {
	Query string `json:"query" description:"Search query: a tag (file:auth.go), event type (command, decision), turn number (turn:5), or text to search for."`
}

// NewRecallTool creates a tool that retrieves full notebook entries by
// tag, event type, turn number, or text search.
func NewRecallTool(svc notebook.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RecallToolName,
		string(recallDescription),
		func(ctx context.Context, params RecallParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Query == "" {
				return fantasy.NewTextErrorResponse("query parameter is required"), nil
			}

			sessionID := getSessionID(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required for recall"), nil
			}

			entries, err := searchNotebook(ctx, svc, sessionID, params.Query)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to search notebook: %v", err)), nil
			}

			if len(entries) == 0 {
				return fantasy.NewTextResponse("No notebook entries found matching the query."), nil
			}

			var sb strings.Builder
			for _, e := range entries {
				sb.WriteString(fmt.Sprintf("## Turn %d.%d — %s\n", e.TurnNumber, e.EventNumber, e.Title))
				// Return the full uncompressed text when available,
				// so recall always provides the original detail even
				// after compaction has replaced entry_text.
				text := e.EntryText
				if e.EntryTextFull != "" {
					text = e.EntryTextFull
				}
				sb.WriteString(text)
				if len(e.Tags) > 0 {
					sb.WriteString("\nTags: ")
					sb.WriteString(strings.Join(e.Tags, " "))
				}
				sb.WriteString("\n\n---\n\n")
			}
			return fantasy.NewTextResponse(sb.String()), nil
		},
	)
}

// searchNotebook dispatches a query to the appropriate search method
// based on the query prefix.
func searchNotebook(ctx context.Context, svc notebook.Service, sessionID, query string) ([]notebook.Entry, error) {
	// Normalize: strip a leading # if present so both "#file:auth.go"
	// and "file:auth.go" match the same stored tags.
	query = strings.TrimPrefix(query, "#")
	switch {
	case strings.HasPrefix(query, "file:"):
		return svc.SearchByTag(ctx, sessionID, query)
	case strings.HasPrefix(query, "phase:"):
		return svc.SearchByTag(ctx, sessionID, query)
	case strings.HasPrefix(query, "turn:"):
		var turn int64
		if n, _ := fmt.Sscanf(query, "turn:%d", &turn); n != 1 {
			return nil, fmt.Errorf("invalid turn: query %q: expected turn:<number>", query)
		}
		return svc.GetByTurn(ctx, sessionID, turn)
	case query == "command" || query == "decision" || query == "file_read" || query == "file_edit" || query == "exploration":
		return svc.SearchByEventType(ctx, sessionID, query)
	default:
		return svc.SearchByText(ctx, sessionID, query)
	}
}
