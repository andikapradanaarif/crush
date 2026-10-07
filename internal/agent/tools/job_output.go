package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/shell"
)

const (
	JobOutputToolName = "job_output"
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes before returning output"`
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

func NewJobOutputTool(spillDir string, cmdLog cmdlog.Service) fantasy.AgentTool {
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
				bgShell.WaitContext(ctx)
			}

			stdout, stderr, done, err := bgShell.GetOutput()

			// A job that outlived the synchronous wait lands here:
			// its completion is the first real verdict the ledger
			// sees, so a 90s `go test` still reaches command memory.
			// TakeRecorded keeps one write per job across poll calls.
			if done && cmdLog != nil && bgShell.TakeRecorded() {
				interrupted := shell.IsInterrupt(err)
				cmdLog.RecordRun(ctx, cmdlog.Run{
					SessionID:      GetSessionFromContext(ctx),
					ToolCallID:     call.ID,
					Command:        bgShell.Command,
					CWD:            bgShell.Shell.GetWorkingDir(),
					Stdout:         stdout,
					Stderr:         stderr,
					Err:            err,
					ExitCode:       shell.ExitCode(err),
					Ran:            shell.IsExitStatus(err),
					Interrupted:    interrupted,
					ComponentExits: bgShell.Shell.TakeComponentExits(),
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
				if exitCode != 0 {
					outputParts = append(outputParts, fmt.Sprintf("Exit code %d", exitCode))
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
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
