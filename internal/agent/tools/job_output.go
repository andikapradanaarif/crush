package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobOutputToolName = "job_output"

	// jobOutputWaitLimit caps how long a wait=true call blocks, so a long
	// job hands control back to the agent instead of stalling the turn.
	// Keep job_output.md, bash.md.tpl, and the wait param in sync.
	jobOutputWaitLimit = 2 * time.Minute
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes, for at most 2 minutes, before returning output"`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory"`
	// ExitCode is the completed command's verdict — marshaled explicitly
	// so done:true + exit_code:0 is distinguishable by key presence.
	// Like bash, a non-zero exit is text output — consumers must read
	// this field, not the result type. Only meaningful when Done.
	ExitCode int `json:"exit_code"`
}

func NewJobOutputTool(spillDir string, cmdLog cmdlog.Service, progress pubsub.Publisher[ToolProgress]) fantasy.AgentTool {
	return newJobOutputTool(spillDir, cmdLog, progress, jobOutputWaitLimit)
}

// newJobOutputTool is NewJobOutputTool with an injectable wait limit, so
// tests need not wait the full two minutes.
func newJobOutputTool(spillDir string, cmdLog cmdlog.Service, progress pubsub.Publisher[ToolProgress], waitLimit time.Duration) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()
			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			if params.Wait {
				waitCtx, cancel := context.WithTimeout(ctx, waitLimit)
				waitWithProgress(waitCtx, bgShell, newProgressReporter(progress, GetSessionFromContext(ctx), call.ID))
				cancel()
			}

			stdout, stderr, done, err := bgShell.GetOutput()

			// A job that outlived the synchronous wait lands here:
			// its completion is the first real verdict the ledger
			// sees, so a 90s `go test` still reaches command memory.
			// TakeRecorded keeps one write per job across poll calls.
			if done && cmdLog != nil && bgShell.TakeRecorded() {
				interrupted := shell.IsInterrupt(err)
				cmdLog.RecordRun(ctx, cmdlog.Run{
					SessionID:   GetSessionFromContext(ctx),
					ToolCallID:  call.ID,
					Command:     bgShell.Command,
					CWD:         bgShell.Shell.GetWorkingDir(),
					Stdout:      stdout,
					Stderr:      stderr,
					Err:         err,
					ExitCode:    shell.ExitCode(err),
					Ran:         shell.IsExitStatus(err),
					Interrupted: interrupted,
					HeadBefore:  bgShell.HeadBefore,
					Components:  bgShell.Shell.TakeComponents(),
				})
			}

			var outputParts []string
			if stdout != "" {
				outputParts = append(outputParts, stdout)
			}
			if stderr != "" {
				outputParts = append(outputParts, stderr)
			}

			status := "running"
			exitCode := 0
			if done {
				status = "completed"
				exitCode = shell.ExitCode(err)
				if msg := shell.Describe(err); msg != "" {
					outputParts = append(outputParts, msg)
				}
			}

			output := strings.Join(outputParts, "\n")
			output = TruncateOutput(output, spillDir)

			metadata := JobOutputResponseMetadata{
				ShellID:          params.ShellID,
				Command:          bgShell.Command,
				Description:      bgShell.Description,
				Done:             done,
				WorkingDirectory: bgShell.WorkingDir,
				ExitCode:         exitCode,
			}

			if output == "" {
				output = BashNoOutput
			}

			result := fmt.Sprintf("Status: %s\n\n%s", status, output)
			if params.Wait && !done && ctx.Err() == nil {
				result += "\n\nStopped waiting: the job is still running and the output above is what it has printed so far. Keep working on other things and check back; wait again only if nothing else is left to do."
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}

// waitWithProgress blocks until the shell finishes or ctx is cancelled,
// publishing live output while it waits.
func waitWithProgress(ctx context.Context, bgShell *shell.BackgroundShell, progress *progressReporter) {
	select {
	case <-ctx.Done():
		return
	case <-bgShell.Done():
		return
	default:
	}
	progress.report(bgShell)

	ticker := time.NewTicker(progressPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bgShell.Done():
			return
		case <-ticker.C:
			progress.report(bgShell)
		}
	}
}
