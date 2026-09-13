package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

type DiagnosticsParams struct {
	FilePath string `json:"file_path,omitempty" description:"The path to the file to get diagnostics for (leave empty for project diagnostics)"`
}

const DiagnosticsToolName = "lsp_diagnostics"

//go:embed diagnostics.md
var diagnosticsDescription string

func NewDiagnosticsTool(lspManager *lsp.Manager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		DiagnosticsToolName,
		diagnosticsDescription,
		func(ctx context.Context, params DiagnosticsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			NotifyLSPs(ctx, lspManager, params.FilePath)
			output := FormatDiagnostics(params.FilePath, lspManager)
			return fantasy.NewTextResponse(output), nil
		},
	)
}

// openInLSPs ensures LSP servers are running and aware of the file, but does
// not notify changes or wait for fresh diagnostics. Use this for read-only
// operations like view where the file content hasn't changed.
func openInLSPs(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
) {
	if filepath == "" || manager == nil {
		return
	}

	manager.Start(ctx, filepath)

	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		_ = client.OpenFileOnDemand(ctx, filepath)
	}
}

// waitForLSPDiagnostics waits briefly for diagnostics publication after a file
// has been opened. Intended for read-only situations where viewing up-to-date
// files matters but latency should remain low (i.e. when using the view tool).
func waitForLSPDiagnostics(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
	timeout time.Duration,
) {
	if filepath == "" || manager == nil || timeout <= 0 {
		return
	}

	var wg sync.WaitGroup
	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		wg.Go(func() {
			client.WaitForDiagnostics(ctx, timeout)
		})
	}
	wg.Wait()
}

// NotifyLSPs notifies LSP servers that a file has changed and waits for
// updated diagnostics. Use this after edit/multiedit operations.
// When filepath is empty, refreshes all open files across all LSP clients
// and sends a workspace-level change notification for full re-analysis.
func NotifyLSPs(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
) {
	if manager == nil {
		return
	}
	if filepath == "" {
		// No specific file — refresh all open files for all clients.
		var wg sync.WaitGroup
		for client := range manager.Clients().Seq() {
			wg.Go(func() {
				client.RefreshOpenFiles(ctx)
				if err := client.NotifyWorkspaceChange(ctx); err != nil {
					slog.WarnContext(ctx, "Failed to notify workspace change", "error", err)
				}
				client.WaitForDiagnostics(ctx, 5*time.Second)
			})
		}
		wg.Wait()
		return
	}

	manager.Start(ctx, filepath)

	var wg sync.WaitGroup
	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		_ = client.OpenFileOnDemand(ctx, filepath)
		_ = client.NotifyChange(ctx, filepath)
		wg.Go(func() {
			client.WaitForDiagnostics(ctx, 5*time.Second)
		})
	}
	wg.Wait()
}

// FormatDiagnostics renders the file and project diagnostics as a
// formatted, sorted, truncated string for tool-result output.
func FormatDiagnostics(filePath string, manager *lsp.Manager) string {
	if manager == nil {
		return ""
	}

	var fileDiags []string
	var projectDiags []string

	for lspName, client := range manager.Clients().Seq2() {
		for location, diags := range client.GetDiagnostics() {
			path, err := location.Path()
			if err != nil {
				slog.Error("Failed to convert diagnostic location URI to path", "uri", location, "error", err)
				continue
			}
			isCurrentFile := path == filePath
			for _, diag := range diags {
				formattedDiag := formatDiagnostic(path, diag, lspName)
				if isCurrentFile {
					fileDiags = append(fileDiags, formattedDiag)
				} else {
					projectDiags = append(projectDiags, formattedDiag)
				}
			}
		}
	}

	sortDiagnostics(fileDiags)
	sortDiagnostics(projectDiags)

	var output strings.Builder
	writeDiagnostics(&output, "file_diagnostics", fileDiags)
	writeDiagnostics(&output, "project_diagnostics", projectDiags)

	if len(fileDiags) > 0 || len(projectDiags) > 0 {
		fileErrors := countSeverity(fileDiags, "Error")
		fileWarnings := countSeverity(fileDiags, "Warn")
		projectErrors := countSeverity(projectDiags, "Error")
		projectWarnings := countSeverity(projectDiags, "Warn")
		output.WriteString("\n<diagnostic_summary>\n")
		if filePath != "" {
			fmt.Fprintf(&output, "Current file: %d errors, %d warnings\n", fileErrors, fileWarnings)
		}
		fmt.Fprintf(&output, "Project: %d errors, %d warnings\n", projectErrors, projectWarnings)
		output.WriteString("</diagnostic_summary>\n")
	}

	out := output.String()
	slog.Debug("Diagnostics", "output", out)
	return out
}

func writeDiagnostics(output *strings.Builder, tag string, in []string) {
	if len(in) == 0 {
		return
	}
	output.WriteString("\n<" + tag + ">\n")
	if len(in) > 10 {
		output.WriteString(strings.Join(in[:10], "\n"))
		fmt.Fprintf(output, "\n... and %d more diagnostics", len(in)-10)
	} else {
		output.WriteString(strings.Join(in, "\n"))
	}
	output.WriteString("\n</" + tag + ">\n")
}

func sortDiagnostics(in []string) []string {
	sort.Slice(in, func(i, j int) bool {
		iIsError := strings.HasPrefix(in[i], "Error")
		jIsError := strings.HasPrefix(in[j], "Error")
		if iIsError != jIsError {
			return iIsError // Errors come first
		}
		return in[i] < in[j] // Then alphabetically
	})
	return in
}

func formatDiagnostic(pth string, diagnostic protocol.Diagnostic, source string) string {
	severity := "Info"
	switch diagnostic.Severity {
	case protocol.SeverityError:
		severity = "Error"
	case protocol.SeverityWarning:
		severity = "Warn"
	case protocol.SeverityHint:
		severity = "Hint"
	}

	location := fmt.Sprintf("%s:%d:%d", pth, diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1)

	sourceInfo := source
	if diagnostic.Source != "" {
		sourceInfo += " " + diagnostic.Source
	}

	codeInfo := ""
	if diagnostic.Code != nil {
		codeInfo = fmt.Sprintf("[%v]", diagnostic.Code)
	}

	tagsInfo := ""
	if len(diagnostic.Tags) > 0 {
		var tags []string
		for _, tag := range diagnostic.Tags {
			switch tag {
			case protocol.Unnecessary:
				tags = append(tags, "unnecessary")
			case protocol.Deprecated:
				tags = append(tags, "deprecated")
			}
		}
		if len(tags) > 0 {
			tagsInfo = fmt.Sprintf(" (%s)", strings.Join(tags, ", "))
		}
	}

	return fmt.Sprintf("%s: %s [%s]%s%s %s",
		severity,
		location,
		sourceInfo,
		codeInfo,
		tagsInfo,
		diagnostic.Message)
}

func countSeverity(diagnostics []string, severity string) int {
	count := 0
	for _, diag := range diagnostics {
		if strings.HasPrefix(diag, severity) {
			count++
		}
	}
	return count
}

// DiagnosticsSnapshot is a multiset of error-severity diagnostics across
// all LSP clients, keyed by a stable identity. It is the input for the
// before/after delta a verifying decorator computes around a mutation.
type DiagnosticsSnapshot map[string]int

// SnapshotDiagnostics returns the current project-wide error diagnostics
// as a multiset keyed by "path|line|character|message".
func SnapshotDiagnostics(manager *lsp.Manager) DiagnosticsSnapshot {
	snapshot := DiagnosticsSnapshot{}
	if manager == nil {
		return snapshot
	}
	for client := range manager.Clients().Seq() {
		for location, diags := range client.GetDiagnostics() {
			path, err := location.Path()
			if err != nil {
				slog.Error("Failed to convert diagnostic location URI to path", "uri", location, "error", err)
				continue
			}
			for _, diag := range diags {
				if diag.Severity != protocol.SeverityError {
					continue
				}
				key := fmt.Sprintf("%s|%d|%d|%s", path,
					diag.Range.Start.Line, diag.Range.Start.Character,
					diag.Message)
				snapshot[key]++
			}
		}
	}
	return snapshot
}

// NewErrorsSince returns the diagnostic keys present in s beyond the
// counts recorded in baseline — the multiset difference.
func (s DiagnosticsSnapshot) NewErrorsSince(baseline DiagnosticsSnapshot) []string {
	var newErrs []string
	for key, count := range s {
		for range count - baseline[key] {
			newErrs = append(newErrs, key)
		}
	}
	return newErrs
}

// AnyClientHandles reports whether any running LSP client claims the
// file. When false, a diagnostics-delta check does not apply.
func AnyClientHandles(manager *lsp.Manager, filepath string) bool {
	if manager == nil || filepath == "" {
		return false
	}
	for client := range manager.Clients().Seq() {
		if client.HandlesFile(filepath) {
			return true
		}
	}
	return false
}

// PrepareDiagnosticsBaseline ensures the file is open in its LSP clients
// and waits briefly for initial diagnostics — the pre-mutation step so a
// never-opened file does not read an empty baseline.
func PrepareDiagnosticsBaseline(ctx context.Context, manager *lsp.Manager, filepath string, timeout time.Duration) {
	openInLSPs(ctx, manager, filepath)
	waitForLSPDiagnostics(ctx, manager, filepath, timeout)
}
