package agent

import (
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
)

// MCP instruction hard limits. MCP InitializeResult.Instructions is
// free-form server-controlled text; without caps a single verbose
// server can bloat every request.
const (
	// maxMCPInstructionsPerServer caps each server's instructions at
	// ~1K estimated tokens.
	maxMCPInstructionsPerServer = 1_000
	// maxMCPInstructionsTotal caps the combined block at ~4K estimated
	// tokens.
	maxMCPInstructionsTotal = 4_000
	// markerReserveTokens approximates the keep-ends "[...truncated...]"
	// marker overhead so the marker itself doesn't push output past the
	// total cap; budgets at or below it are treated as exhausted.
	markerReserveTokens = 8
)

// collectMCPInstructions gathers InitializeResult instructions from
// every connected MCP server, sorted by server name for deterministic
// output, capped per-server and in total.
func collectMCPInstructions() string {
	states := mcp.GetStates()
	raw := make(map[string]string, len(states))
	for name, server := range states {
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			raw[name] = s
		}
	}
	return capMCPInstructions(raw)
}

// capMCPInstructions concatenates per-server instructions in
// deterministic name order, truncating over-limit servers (keeping the
// beginning and end) and skipping servers that no longer fit the total
// budget. Alphabetical processing means the total budget is claimed in
// name order — late-sorted servers lose first. Every truncation is
// logged with the server identity.
func capMCPInstructions(raw map[string]string) string {
	names := slices.Sorted(maps.Keys(raw))

	var out strings.Builder
	var total int64
	for _, name := range names {
		s := strings.ToValidUTF8(raw[name], "")
		if est := approxTokenCount(s); est > maxMCPInstructionsPerServer {
			slog.Warn("MCP server instructions exceed per-server limit; truncating",
				"server", name,
				"est_tokens", est,
				"limit", maxMCPInstructionsPerServer)
			s = prompt.TruncateToTokenLimitKeepEnds(s, maxMCPInstructionsPerServer)
		}
		est := approxTokenCount(s)
		if total+est > maxMCPInstructionsTotal {
			remaining := maxMCPInstructionsTotal - total
			if remaining <= markerReserveTokens {
				// The keep-ends marker costs ~markerReserveTokens; a
				// remaining budget that small can't hold a useful
				// excerpt without overshooting the cap.
				slog.Warn("Skipping MCP server instructions; total cap reached",
					"server", name,
					"total_limit", maxMCPInstructionsTotal)
				continue
			}
			slog.Warn("MCP server instructions exceed remaining total budget; truncating",
				"server", name,
				"est_tokens", est,
				"remaining", remaining)
			s = prompt.TruncateToTokenLimitKeepEnds(s, int(remaining-markerReserveTokens))
			est = approxTokenCount(s)
		}
		out.WriteString(s)
		out.WriteString("\n\n")
		total += est
	}
	return out.String()
}
