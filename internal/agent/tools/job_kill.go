package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobKillToolName = "job_kill"
)

//go:embed job_kill.md
var jobKillDescription string

type JobKillParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to terminate"`
}

type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

func NewJobKillTool(cmdLog cmdlog.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobKillToolName,
		jobKillDescription,
		func(ctx context.Context, params JobKillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()

			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			metadata := JobKillResponseMetadata{
				ShellID:     params.ShellID,
				Command:     bgShell.Command,
				Description: bgShell.Description,
			}

			err := bgManager.Kill(params.ShellID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// The kill is an observation point: record the interrupt
			// — or the real verdict if the job finished just before
			// the kill landed. TakeRecorded keeps it exactly-once.
			if cmdLog != nil && bgShell.TakeRecorded() {
				stdout, stderr, done, exitErr := bgShell.GetOutput()
				run := cmdlog.Run{
					SessionID:   GetSessionFromContext(ctx),
					Command:     bgShell.Command,
					CWD:         bgShell.Shell.GetWorkingDir(),
					Stdout:      stdout,
					Stderr:      stderr,
					Err:         ctx.Err(),
					Interrupted: true,
				}
				if done {
					run.Err = exitErr
					run.ExitCode = shell.ExitCode(exitErr)
					run.Ran = shell.IsExitStatus(exitErr)
					run.Interrupted = shell.IsInterrupt(exitErr)
				}
				cmdLog.RecordRun(ctx, run)
			}

			result := fmt.Sprintf("Background shell %s terminated successfully", params.ShellID)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
