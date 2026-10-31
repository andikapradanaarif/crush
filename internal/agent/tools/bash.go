package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/toolclass"
)

type BashParams struct {
	Description         string `json:"description" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	Command             string `json:"command" description:"The command to execute"`
	WorkingDir          string `json:"working_dir,omitempty" description:"The working directory to execute the command in (defaults to current directory)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Set to true (boolean) to run this command in the background. Use job_output to read the output later."`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty" description:"Seconds to wait before automatically moving the command to a background job (default: 60)"`
}

type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
}

type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
	Background       bool   `json:"background,omitempty"`
	ShellID          string `json:"shell_id,omitempty"`
	// Done/ExitCode record the run's verdict when the command completed
	// within the tool call. A non-zero exit is reported as text output,
	// not an error response — consumers that need the verdict (e.g. the
	// verification gate) must read exit_code, not the result type. Both
	// marshal explicitly (no omitempty) so done:true + exit_code:0 is
	// distinguishable from a still-running command by a consumer that
	// checks key presence.
	Done     bool `json:"done"`
	ExitCode int  `json:"exit_code"`
}

const (
	BashToolName = "bash"

	DefaultAutoBackgroundAfter = 60 // Commands taking longer automatically become background jobs
	MaxOutputLength            = 30000
	BashNoOutput               = "no output"
)

//go:embed bash.md.tpl
var bashDescriptionTmpl []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescriptionTmpl)),
)

type bashDescriptionData struct {
	BannedCommands  string
	MaxOutputLength int
	Attribution     config.Attribution
	ModelID         string
	RgAvailable     bool
	GhAvailable     bool
}

// defaultBlockedCommands is the built-in dangerous-command list. It is a
// default, not a hardcoded policy: the `permissions` config block can add
// to it and take away from it (see resolveBlockedCommands).
var defaultBlockedCommands = []string{
	// Network/Download tools
	"alias",
	"aria2c",
	"axel",
	"chrome",
	"curl",
	"curlie",
	"firefox",
	"http-prompt",
	"httpie",
	"links",
	"lynx",
	"nc",
	"safari",
	"scp",
	"ssh",
	"telnet",
	"w3m",
	"wget",
	"xh",

	// System administration
	"doas",
	"su",
	"sudo",

	// Indirect execution: these run a command name the static check can't
	// see, so whatever they wrap would never be flagged on its own.
	"env",
	"eval",
	"nohup",
	"xargs",

	// Package managers
	"apk",
	"apt",
	"apt-cache",
	"apt-get",
	"dnf",
	"dpkg",
	"emerge",
	"home-manager",
	"makepkg",
	"opkg",
	"pacman",
	"paru",
	"pkg",
	"pkg_add",
	"pkg_delete",
	"portage",
	"rpm",
	"yay",
	"yum",
	"zypper",

	// System modification
	"at",
	"batch",
	"chkconfig",
	"crontab",
	"fdisk",
	"mkfs",
	"mount",
	"parted",
	"service",
	"systemctl",
	"umount",

	// Network configuration
	"firewall-cmd",
	"ifconfig",
	"ip",
	"iptables",
	"netstat",
	"pfctl",
	"route",
	"ufw",
}

// resolveBlockedCommands returns the effective dangerous-command list:
// the built-in defaults plus perms.BlockedCommands, minus
// perms.AllowedCommands.
//
// The built-in order is preserved and user additions are appended. The list is
// embedded verbatim in the tool description, so it has to be deterministic, but
// sorting it would scramble the category grouping the defaults are written in
// and churn the prompt for every existing user.
func resolveBlockedCommands(perms *config.Permissions) []string {
	if perms == nil {
		return slices.Clone(defaultBlockedCommands)
	}
	// Both sides are normalized the way the block list itself matches, so a
	// config entry spelled "/usr/bin/CURL" or " curl " lines up with the
	// "curl" already on the list. Without this, allowing a command by a
	// slightly different spelling did nothing at all, which is the worst way
	// for a permission setting to be wrong: it looks applied.
	allowed := make(map[string]struct{}, len(perms.AllowedCommands))
	for _, cmd := range perms.AllowedCommands {
		allowed[shell.NormalizeCommandName(cmd)] = struct{}{}
	}

	blocked := make([]string, 0, len(defaultBlockedCommands)+len(perms.BlockedCommands))
	blocked = append(blocked, defaultBlockedCommands...)
	for _, cmd := range perms.BlockedCommands {
		if name := shell.NormalizeCommandName(cmd); name != "" {
			blocked = append(blocked, name)
		}
	}
	blocked = slices.DeleteFunc(blocked, func(c string) bool {
		_, ok := allowed[shell.NormalizeCommandName(c)]
		return ok
	})

	seen := make(map[string]struct{}, len(blocked))
	deduped := blocked[:0]
	for _, cmd := range blocked {
		if _, dup := seen[cmd]; dup {
			continue
		}
		seen[cmd] = struct{}{}
		deduped = append(deduped, cmd)
	}
	return deduped
}

func bashDescription(attribution *config.Attribution, modelID string, blocked []string) string {
	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{
		BannedCommands:  strings.Join(blocked, ", "),
		MaxOutputLength: MaxOutputLength,
		Attribution:     *attribution,
		ModelID:         modelID,
		RgAvailable:     getRg() != "",
		GhAvailable:     ghAvailable,
	}); err != nil {
		// this should never happen.
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

// blockFuncs builds the block list from the resolved dangerous commands.
// The flat command list is configurable; the argument blockers below are
// structural rules about specific flags rather than a list of binaries, so
// they stay built in.
func blockFuncs(blocked []string) []shell.BlockFunc {
	return []shell.BlockFunc{
		shell.CommandsBlocker(blocked),

		// System package managers
		shell.ArgumentsBlocker("apk", []string{"add"}, nil),
		shell.ArgumentsBlocker("apt", []string{"install"}, nil),
		shell.ArgumentsBlocker("apt-get", []string{"install"}, nil),
		shell.ArgumentsBlocker("dnf", []string{"install"}, nil),
		shell.ArgumentsBlocker("pacman", nil, []string{"-S"}),
		shell.ArgumentsBlocker("pkg", []string{"install"}, nil),
		shell.ArgumentsBlocker("yum", []string{"install"}, nil),
		shell.ArgumentsBlocker("zypper", []string{"install"}, nil),

		// Language-specific package managers
		shell.ArgumentsBlocker("brew", []string{"install"}, nil),
		shell.ArgumentsBlocker("cargo", []string{"install"}, nil),
		shell.ArgumentsBlocker("gem", []string{"install"}, nil),
		shell.ArgumentsBlocker("go", []string{"install"}, nil),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
		shell.ArgumentsBlocker("pip", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pip3", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"--global"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"-g"}),
		shell.ArgumentsBlocker("yarn", []string{"global", "add"}, nil),

		// `go test -exec` can run arbitrary commands
		shell.ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}),
	}
}

func NewBashTool(lspManager *lsp.Manager, permissions permission.Service, workingDir, spillDir string, attribution *config.Attribution, modelID string, maxBackgroundJobs int, perms *config.Permissions, cmdLog cmdlog.Service) fantasy.AgentTool {
	blocked := resolveBlockedCommands(perms)
	userSafe := userSafeCommands(perms)
	return fantasy.NewAgentTool(
		BashToolName,
		string(bashDescription(attribution, modelID, blocked)),
		func(ctx context.Context, params BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Command == "" {
				return fantasy.NewTextErrorResponse("missing command"), nil
			}

			// Determine working directory
			execWorkingDir := cmp.Or(params.WorkingDir, workingDir)

			safeReadOnly := isSafeReadOnly(params.Command, userSafe)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing shell command")
			}
			// Check whether the command is dangerous so we can surface a
			// warning in the permission dialog.
			var check shell.CommandCheck
			if !safeReadOnly {
				check = shell.CheckCommand(params.Command, blockFuncs(blocked))

				p, err := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        execWorkingDir,
						ToolCallID:  call.ID,
						ToolName:    BashToolName,
						Action:      "execute",
						Description: fmt.Sprintf("Execute command: %s", params.Command),
						Params:      BashPermissionsParams(params),
						Danger:      check.Reason,
						// Path is only the working directory here, so a
						// session grant keyed on it alone would turn one
						// approval of `ls` into standing approval for every
						// later command run in that directory. Key it to the
						// command the user was actually shown.
						GrantKey: params.Command,
					},
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !p {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// Re-blocking a command that was already approved would override
			// the answer that was given, whether the user gave it at a prompt
			// or yolo mode gave it on their behalf. That only applies when the
			// check actually recognised a command by name, though: a warning
			// that merely says "this could not be analysed" names nothing the
			// user could have judged, so approving it is not approval of
			// anything in particular and the block list stays on. Otherwise a
			// stray $(date) anywhere on the line would waive the run-time
			// checks for every command beside it.
			//
			// What the block list can see is narrow: commands this interpreter
			// dispatches itself, including names that only resolve after a
			// glob or variable expands. A command that runs inside a child
			// process is invisible to it.
			//
			// Sysadmin mode is the deliberate exception: it asks for nothing
			// and blocks nothing.
			var blocksToUse []shell.BlockFunc
			if !check.Matched && permissions.PermissionMode() != permission.PermissionModeSysadmin {
				blocksToUse = blockFuncs(blocked)
			}
			return executeBashCommand(ctx, params, execWorkingDir, spillDir, blocksToUse, maxBackgroundJobs, lspManager, cmdLog, sessionID, call.ID)
		},
	)
}

// recordRun writes the finished job to command memory when a service is
// wired and the shell still owns the record — the first consumer to
// TakeRecorded wins, so a job_output read that got there first leaves
// nothing to double-count.
func recordRun(ctx context.Context, cmdLog cmdlog.Service, sessionID, toolCallID string, bgShell *shell.BackgroundShell, stdout, stderr string, execErr error) {
	if cmdLog == nil || !bgShell.TakeRecorded() {
		return
	}
	interrupted := shell.IsInterrupt(execErr)
	cmdLog.RecordRun(ctx, cmdlog.Run{
		SessionID:   sessionID,
		ToolCallID:  toolCallID,
		Command:     bgShell.Command,
		CWD:         bgShell.Shell.GetWorkingDir(),
		Stdout:      stdout,
		Stderr:      stderr,
		Err:         execErr,
		ExitCode:    shell.ExitCode(execErr),
		Ran:         shell.IsExitStatus(execErr),
		Interrupted: interrupted,
		HeadBefore:  bgShell.HeadBefore,
		Components:  bgShell.Shell.TakeComponents(),
	})
}

func executeBashCommand(ctx context.Context, params BashParams, execWorkingDir, spillDir string, blockFuncsToUse []shell.BlockFunc, maxBackgroundJobs int, lspManager *lsp.Manager, cmdLog cmdlog.Service, sessionID, toolCallID string) (fantasy.ToolResponse, error) {
	// If explicitly requested as background, start immediately with detached context
	if params.RunInBackground {
		startTime := time.Now()
		bgManager := shell.GetBackgroundShellManager()
		// The pre-exec stamp rides the job to whichever path
		// records it — sync fast-exit, job_output, or kill.
		headBefore := preRunHead(ctx, cmdLog)
		// Use background context so it continues after tool returns
		bgShell, err := bgManager.Start(context.Background(), execWorkingDir, blockFuncsToUse, params.Command, params.Description, maxBackgroundJobs, headBefore)
		if err != nil {
			return fantasy.NewTextErrorResponse("Failed to start background shell: " + err.Error()), nil
		}

		// Wait a short time to detect fast failures (blocked commands, syntax errors, etc.)
		time.Sleep(1 * time.Second)
		stdout, stderr, done, execErr := bgShell.GetOutput()

		if done {
			// Command failed or completed very quickly
			bgManager.Remove(bgShell.ID)

			interrupted := shell.IsInterrupt(execErr)
			exitCode := shell.ExitCode(execErr)
			if exitCode == 0 && !interrupted && execErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("[Job %s] Error executing command: %s", bgShell.ID, execErr)), nil
			}
			recordRun(ctx, cmdLog, sessionID, toolCallID, bgShell, stdout, stderr, execErr)

			stdout = formatOutput(stdout, stderr, execErr, spillDir)
			stdout += lspDiagnosticsForFailure(params.Command, exitCode, interrupted, lspManager)

			metadata := BashResponseMetadata{
				StartTime:        startTime.UnixMilli(),
				EndTime:          time.Now().UnixMilli(),
				Output:           stdout,
				Description:      params.Description,
				Background:       params.RunInBackground,
				WorkingDirectory: bgShell.WorkingDir,
				Done:             true,
				ExitCode:         exitCode,
			}
			if stdout == "" {
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
			}
			stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(bgShell.WorkingDir))
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
		}

		// Still running after fast-failure check - return as background job
		metadata := BashResponseMetadata{
			StartTime:        startTime.UnixMilli(),
			EndTime:          time.Now().UnixMilli(),
			Description:      params.Description,
			WorkingDirectory: bgShell.WorkingDir,
			Background:       true,
			ShellID:          bgShell.ID,
		}
		response := fmt.Sprintf("Background shell started with ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
	}

	// Start synchronous execution with auto-background support
	startTime := time.Now()

	// Start with detached context so it can survive if moved to background
	bgManager := shell.GetBackgroundShellManager()
	headBefore := preRunHead(ctx, cmdLog)
	bgShell, err := bgManager.Start(context.Background(), execWorkingDir, blockFuncsToUse, params.Command, params.Description, maxBackgroundJobs, headBefore)
	if err != nil {
		return fantasy.NewTextErrorResponse("Failed to start shell: " + err.Error()), nil
	}

	// Wait for either completion, auto-background threshold, or context cancellation
	autoBackgroundAfter := cmp.Or(params.AutoBackgroundAfter, DefaultAutoBackgroundAfter)
	autoBackgroundThreshold := time.Duration(autoBackgroundAfter) * time.Second
	timer := time.NewTimer(autoBackgroundThreshold)
	defer timer.Stop()

	select {
	case <-bgShell.Done():
	case <-timer.C:
	case <-ctx.Done():
		// Incoming context was cancelled before we moved to background
		// Kill the shell and return error
		bgManager.Kill(bgShell.ID)
		if cmdLog != nil && bgShell.TakeRecorded() {
			killOut, killErr, kDone, kErr := bgShell.GetOutput()
			run := cmdlog.Run{
				SessionID:   sessionID,
				ToolCallID:  toolCallID,
				Command:     params.Command,
				CWD:         bgShell.Shell.GetWorkingDir(),
				Stdout:      killOut,
				Stderr:      killErr,
				Err:         ctx.Err(),
				Interrupted: true,
				HeadBefore:  bgShell.HeadBefore,
				Components:  bgShell.Shell.TakeComponents(),
			}
			// The job may have reached its own verdict
			// just before the kill landed — record it.
			if kDone {
				run.Err = kErr
				run.ExitCode = shell.ExitCode(kErr)
				run.Ran = shell.IsExitStatus(kErr)
				run.Interrupted = shell.IsInterrupt(kErr)
			}
			cmdLog.RecordRun(ctx, run)
		}
		return fantasy.ToolResponse{}, ctx.Err()
	}
	stdout, stderr, done, execErr := bgShell.GetOutput()

	if done {
		// Command completed within threshold - return synchronously
		// Remove from background manager since we're returning directly
		// Don't call Kill() as it cancels the context and corrupts the exit code
		bgManager.Remove(bgShell.ID)

		interrupted := shell.IsInterrupt(execErr)
		exitCode := shell.ExitCode(execErr)
		if exitCode == 0 && !interrupted && execErr != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("[Job %s] Error executing command: %s", bgShell.ID, execErr)), nil
		}
		recordRun(ctx, cmdLog, sessionID, toolCallID, bgShell, stdout, stderr, execErr)

		stdout = formatOutput(stdout, stderr, execErr, spillDir)
		stdout += lspDiagnosticsForFailure(params.Command, exitCode, interrupted, lspManager)

		metadata := BashResponseMetadata{
			StartTime:        startTime.UnixMilli(),
			EndTime:          time.Now().UnixMilli(),
			Output:           stdout,
			Description:      params.Description,
			Background:       params.RunInBackground,
			WorkingDirectory: bgShell.WorkingDir,
			Done:             true,
			ExitCode:         exitCode,
		}
		if stdout == "" {
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
		}
		stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(bgShell.WorkingDir))
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
	}

	// Still running - keep as background job
	metadata := BashResponseMetadata{
		StartTime:        startTime.UnixMilli(),
		EndTime:          time.Now().UnixMilli(),
		Description:      params.Description,
		WorkingDirectory: bgShell.WorkingDir,
		Background:       true,
		ShellID:          bgShell.ID,
	}
	response := fmt.Sprintf("Command is taking longer than expected and has been moved to background.\n\nBackground shell ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

// preRunHead stamps HEAD just before the command execs — the
// recorded run then attributes only the movement in [pre, post], so
// an external commit landing between two runs stays outside every
// run's range (#365). Empty without a memory service or outside a
// repo; the run then falls back to the ledger baseline. The read
// sits on the exec critical path — normally ~ms, worst case
// HeadSHA's 3s cap when .git is wedged.
func preRunHead(ctx context.Context, cmdLog cmdlog.Service) string {
	if cmdLog == nil {
		return ""
	}
	return cmdLog.HeadSHA(ctx)
}

// formatOutput formats the output of a completed command with error handling
func formatOutput(stdout, stderr string, execErr error, spillDir string) string {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)

	stdout = TruncateOutput(stdout, spillDir)
	stderr = TruncateOutput(stderr, spillDir)

	errorMessage := stderr
	if errorMessage == "" && execErr != nil {
		errorMessage = execErr.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""

	if hasBothOutputs {
		stdout += "\n"
	}

	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}

	return stdout
}

func normalizeWorkingDir(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ReplaceAll(path, fsext.WindowsWorkingDirDrive(), "")
	}
	return filepath.ToSlash(path)
}

// isBuildOrTestCommand reports whether command invokes a known build,
// test, or lint tool. The vocabulary lives in internal/toolclass so
// command-memory classification shares it.
func isBuildOrTestCommand(command string) bool {
	return toolclass.IsBuildOrTestCommand(command)
}

// lspDiagnosticsForFailure appends current LSP project diagnostics to
// failed build/test command output so the model gets ground truth to
// fix against without a separate lsp_diagnostics call. Returns "" when
// the command is not a known build/test invocation, when the command
// did not fail, or when no diagnostics are available.
func lspDiagnosticsForFailure(command string, exitCode int, interrupted bool, lspManager *lsp.Manager) string {
	if interrupted || exitCode == 0 || lspManager == nil || !isBuildOrTestCommand(command) {
		return ""
	}
	diags := FormatDiagnostics("", lspManager)
	if diags == "" {
		return ""
	}
	return "\n\n" + diags
}
