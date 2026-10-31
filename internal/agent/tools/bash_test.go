package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

type mockBashPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockBashPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockBashPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockBashPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockBashPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockBashPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockBashPermissionService) SetSessionAutoApprove(sessionID string, approve bool) {}
func (m *mockBashPermissionService) ClearSessionAutoApprove(sessionID string)             {}

func (m *mockBashPermissionService) SessionAutoApproves(sessionID string) bool {
	return false
}

func (m *mockBashPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (m *mockBashPermissionService) PendingRequests() []permission.PermissionRequest {
	return nil
}

func (m *mockBashPermissionService) SetSkipRequests(skip bool) {}

func (m *mockBashPermissionService) SkipRequests() bool {
	return false
}

func (m *mockBashPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func (m *mockBashPermissionService) SetPermissionMode(mode permission.PermissionMode) {}

func (m *mockBashPermissionService) PermissionMode() permission.PermissionMode {
	return permission.PermissionModeNormal
}

func (m *mockBashPermissionService) SubscribeModeChanges(ctx context.Context) <-chan pubsub.Event[permission.ModeChangedEvent] {
	return make(<-chan pubsub.Event[permission.ModeChangedEvent])
}

func TestBashTool_DefaultAutoBackgroundThreshold(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "default threshold",
		Command:     "echo done",
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.False(t, meta.Background)
	require.Empty(t, meta.ShellID)
	require.Contains(t, meta.Output, "done")
}

func TestBashTool_CustomAutoBackgroundThreshold(t *testing.T) {
	workingDir := t.TempDir()
	tool := newBashToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description:         "custom threshold",
		Command:             "sleep 1.5 && echo done",
		AutoBackgroundAfter: 1,
	})

	require.False(t, resp.IsError)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Background)
	require.NotEmpty(t, meta.ShellID)
	require.Contains(t, resp.Content, "moved to background")

	bgManager := shell.GetBackgroundShellManager()
	require.NoError(t, bgManager.Kill(meta.ShellID))
}

type recordingPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
	requestCount int
	allow        bool
	mode         permission.PermissionMode
	lastDanger   string
}

func (m *recordingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.requestCount++
	m.lastDanger = req.Danger
	return m.allow, nil
}

func (m *recordingPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *recordingPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *recordingPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *recordingPermissionService) AutoApproveSession(sessionID string) {}

func (m *recordingPermissionService) SetSessionAutoApprove(sessionID string, approve bool) {}
func (m *recordingPermissionService) ClearSessionAutoApprove(sessionID string)             {}

func (m *recordingPermissionService) SessionAutoApproves(sessionID string) bool {
	return false
}

func (m *recordingPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (m *recordingPermissionService) PendingRequests() []permission.PermissionRequest {
	return nil
}

func (m *recordingPermissionService) SetSkipRequests(skip bool) {}

func (m *recordingPermissionService) SkipRequests() bool {
	return false
}

func (m *recordingPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

func (m *recordingPermissionService) SetPermissionMode(mode permission.PermissionMode) { m.mode = mode }

func (m *recordingPermissionService) PermissionMode() permission.PermissionMode { return m.mode }

func (m *recordingPermissionService) SubscribeModeChanges(ctx context.Context) <-chan pubsub.Event[permission.ModeChangedEvent] {
	return make(<-chan pubsub.Event[permission.ModeChangedEvent])
}

func newBashToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(nil, permissions, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
}

func newBashToolWithRecordingPerms(workingDir string, allow bool) (fantasy.AgentTool, *recordingPermissionService) {
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  allow,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil), perms
}

func TestBashTool_ChainedCommandsRequirePermission(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, true)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// A chain is judged by its parts, so joining two read-only commands is
	// no more of a prompt than either one alone would be.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls",
		Command:     "ls && echo done",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 0, perms.requestCount, "a chain of read-only commands should not prompt")

	// One part that writes is enough to require approval for the whole chain.
	perms.requestCount = 0
	resp = runBashTool(t, tool, ctx, BashParams{
		Description: "chained touch",
		Command:     "ls && touch newfile",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 1, perms.requestCount, "a chain containing a write must prompt")

	// Plain ls should NOT trigger permission check.
	perms.requestCount = 0
	resp = runBashTool(t, tool, ctx, BashParams{
		Description: "plain ls",
		Command:     "ls -la",
	})

	require.False(t, resp.IsError)
	require.Equal(t, 0, perms.requestCount, "plain ls should not trigger permission request")
}

func TestBashTool_ChainedCommandsDenied(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, false)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "chained ls denied",
		Command:     "ls && rm -rf /",
	})

	require.Equal(t, 1, perms.requestCount)
	require.Contains(t, resp.Content, "User denied permission")
}

func TestIsDangerousCommand(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		dangerous bool
	}{
		{
			name:      "simple banned command - curl",
			command:   "curl https://example.com",
			dangerous: true,
		},
		{
			name:      "simple banned command - sudo",
			command:   "sudo apt-get update",
			dangerous: true,
		},
		{
			name:      "npm global install with --global",
			command:   "npm install --global typescript",
			dangerous: true,
		},
		{
			name:      "npm global install with -g",
			command:   "npm install -g typescript",
			dangerous: true,
		},
		{
			name:      "npm local install",
			command:   "npm install typescript",
			dangerous: false,
		},
		{
			name:      "go test with -exec",
			command:   "go test -exec ./malicious ./...",
			dangerous: true,
		},
		{
			name:      "go test without -exec",
			command:   "go test ./...",
			dangerous: false,
		},
		{
			name:      "safe command - ls",
			command:   "ls -la",
			dangerous: false,
		},
		{
			name:      "safe command - echo",
			command:   "echo hello",
			dangerous: false,
		},
		{
			name:      "safe command - git",
			command:   "git status",
			dangerous: false,
		},
		{
			name:      "pip install with --user",
			command:   "pip install --user requests",
			dangerous: true,
		},
		{
			name:      "pip install without --user",
			command:   "pip install requests",
			dangerous: false,
		},
		{
			name:      "brew install",
			command:   "brew install wget",
			dangerous: true,
		},
		{
			// Quoting the command name must not evade detection.
			name:      "quoted dangerous command name",
			command:   `"brew" install wget`,
			dangerous: true,
		},
		{
			// Quoted arguments must still be seen by argument blockers.
			name:      "quoted dangerous argument",
			command:   `go test "-exec" ./malicious ./...`,
			dangerous: true,
		},
		{
			// Command substitution can't be resolved without executing, so
			// it is treated conservatively as dangerous.
			name:      "command substitution is conservative",
			command:   "$(echo brew) install wget",
			dangerous: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shell.IsCommandBlocked(tt.command, blockFuncs(defaultBlockedCommands))
			require.Equal(t, tt.dangerous, result, "command: %s", tt.command)
		})
	}
}

func runBashTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params BashParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  BashToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

func TestTruncateOutputValidUTF8(t *testing.T) {
	t.Parallel()
	// CJK characters are 2 cells wide; this string is far wider than
	// MaxOutputLength so TruncateOutput must truncate it.
	content := strings.Repeat("你好世界", MaxOutputLength)

	out := TruncateOutput(content, t.TempDir())
	require.True(t, utf8.ValidString(out), "truncated output must stay valid UTF-8")
	require.Contains(t, out, "truncated at capture")
}

func TestTruncateOutputShortContent(t *testing.T) {
	t.Parallel()
	content := "short output"
	require.Equal(t, content, TruncateOutput(content, t.TempDir()))
}

func TestTruncateOutputEmoji(t *testing.T) {
	t.Parallel()
	// Emoji with ZWJ sequences should not be split.
	content := strings.Repeat("👨‍👩‍👧‍👦", MaxOutputLength)

	out := TruncateOutput(content, t.TempDir())
	require.True(t, utf8.ValidString(out), "truncated output must stay valid UTF-8")
	require.Contains(t, out, "truncated at capture")
}

func TestIsBuildOrTestCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cmd  string
		want bool
	}{
		{"go build ./...", true},
		{"go test -run Foo ./pkg", true},
		{"go vet .", true},
		{"go run main.go", false},
		{"go fmt .", false},
		{"cargo test", true},
		{"cargo clippy -- -D warnings", true},
		{"npm test", true},
		{"npm run build", true},
		{"npm run lint", true},
		{"npm run dev", false},
		{"npm install", false},
		{"pnpm run typecheck", true},
		{"make", true},
		{"task build", true},
		{"pytest -x", true},
		{"tsc --noEmit", true},
		{"golangci-lint run", true},
		{"cmake --build build", true},
		{"./gradlew test", true},
		{"cd pkg && go test ./...", true},
		{"echo ok; go build .", true},
		{"CGO_ENABLED=0 go test ./...", true},
		{"env X=1 pytest", true},
		{"env -i go test ./...", true},
		{"nice -n 5 make", true},
		{"sudo -u root make test", true},
		{"sudo make", true},
		{"time make test", true},
		{"GOFLAGS=-race go run main.go", false},
		{"FOO=bar ls", false},
		{"ls -la", false},
		{"git status", false},
		{"", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, isBuildOrTestCommand(tc.cmd), "command: %q", tc.cmd)
	}
}

func TestLSPDiagnosticsForFailure(t *testing.T) {
	t.Parallel()

	// Nil manager never produces diagnostics.
	require.Empty(t, lspDiagnosticsForFailure("go test ./...", 1, false, nil))
	// Non-build/test commands are ignored.
	require.Empty(t, lspDiagnosticsForFailure("ls -la", 1, false, nil))
	// Success and interrupts produce nothing.
	require.Empty(t, lspDiagnosticsForFailure("go build .", 0, false, nil))
	require.Empty(t, lspDiagnosticsForFailure("go build .", 130, true, nil))
}

func TestTruncateOutputANSISafe(t *testing.T) {
	t.Parallel()

	esc := "\x1b[38;5;200m" // 12-byte SGR sequence.
	half := MaxOutputLength / 2

	// Head cut straddles the escape: it must land whole in the
	// omitted middle, not split into a partial sequence.
	content := strings.Repeat("x", half-5) + esc + strings.Repeat("y", MaxOutputLength)
	out := TruncateOutput(content, t.TempDir())
	require.True(t, utf8.ValidString(out))
	parts := strings.SplitN(out, "\n\n... [", 2)
	require.Len(t, parts, 2)
	require.NotContains(t, parts[0], "\x1b", "head must not end inside an ANSI escape")

	// Tail cut straddles the escape: the tail must start after the
	// escape's terminator, not mid-sequence.
	total := MaxOutputLength * 2
	tailStart := total - half
	content = strings.Repeat("z", tailStart-3) + esc + strings.Repeat("w", total-tailStart+3-len(esc))
	out = TruncateOutput(content, t.TempDir())
	parts = strings.SplitN(out, "] ...\n\n", 2)
	require.Len(t, parts, 2)
	tail := parts[1]
	require.NotContains(t, tail[:len(esc)], "\x1b[38;5;2",
		"tail must not begin inside an ANSI escape")
}

// recordingCmdLog captures the Run struct bash hands to command
// memory — the seam where ExitCode/Ran/Interrupted/CWD get derived
// from the shell's execErr. headBefore is what HeadSHA returns so
// tests can pin the pre-run stamp's trip into Run.HeadBefore.
type recordingCmdLog struct {
	runs       []cmdlog.Run
	headBefore string
}

func (r *recordingCmdLog) RecordRun(_ context.Context, run cmdlog.Run) {
	r.runs = append(r.runs, run)
}

func (r *recordingCmdLog) HeadSHA(context.Context) string { return r.headBefore }

func (r *recordingCmdLog) ListCommands(context.Context, int) ([]cmdlog.Command, error) {
	return nil, nil
}

func (r *recordingCmdLog) ListOpenFailures(context.Context, int) ([]cmdlog.Failure, error) {
	return nil, nil
}

func (r *recordingCmdLog) ListResolvedFailures(context.Context, int) ([]cmdlog.Failure, error) {
	return nil, nil
}

func (r *recordingCmdLog) ListSessionOpenFailures(context.Context, string) ([]cmdlog.Failure, error) {
	return nil, nil
}

func (r *recordingCmdLog) MarkSuggested(string, string)     {}
func (r *recordingCmdLog) MarkSuggestedFile(string, string) {}
func (r *recordingCmdLog) WasSuggestedFile(context.Context, string, string) bool {
	return false
}

func (r *recordingCmdLog) RecordReferentEpisode(context.Context, cmdlog.ReferentEpisode, int) error {
	return nil
}

func (r *recordingCmdLog) RecordReferentEpisodeLabeled(context.Context, cmdlog.ReferentEpisode, cmdlog.ReferentEpisodeLabelInputs, int) error {
	return nil
}

func (r *recordingCmdLog) LabelReferentEpisode(context.Context, cmdlog.ReferentEpisode, cmdlog.ReferentEpisodeLabelInputs) error {
	return nil
}

func (r *recordingCmdLog) SettleReferentLabels(context.Context, int) error { return nil }

func (r *recordingCmdLog) RecordTurnOutcome(context.Context, cmdlog.TurnOutcome) error {
	return nil
}

func (r *recordingCmdLog) HasTurnOutcome(context.Context, string, string) (bool, error) {
	return false, nil
}

func (r *recordingCmdLog) ListRecentSessionIDs(context.Context, string, int) ([]db.ListRecentSessionsRow, error) {
	return nil, nil
}

func (r *recordingCmdLog) SettleTurnOutcomes(context.Context, int) error { return nil }

func (r *recordingCmdLog) ListReferentCandidates(context.Context, []string, int) ([]cmdlog.Referent, error) {
	return nil, nil
}

func (r *recordingCmdLog) UpdateProjectRates(context.Context, float64) error { return nil }

func (r *recordingCmdLog) ReferentRateLowerBound(context.Context, float64) (float64, error) {
	return 1, nil
}

func (r *recordingCmdLog) RefreshSessionDigests(context.Context, int) error { return nil }

func (r *recordingCmdLog) SearchSessionDigests(context.Context, string, string, int) ([]cmdlog.SessionDigest, error) {
	return nil, nil
}

func (r *recordingCmdLog) RecentSessionDigests(context.Context, string, int) ([]cmdlog.SessionDigest, error) {
	return nil, nil
}

func (r *recordingCmdLog) DeleteSessionDigest(context.Context, string) error { return nil }
func (r *recordingCmdLog) ProjectKey() string                                { return "" }
func (r *recordingCmdLog) ParamVersion() string                              { return "" }
func (r *recordingCmdLog) ForgetSession(string)                              {}

func (r *recordingCmdLog) ListReferentEpisodeSourceIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

func TestBashTool_RecordsRunToCmdLog(t *testing.T) {
	workingDir := t.TempDir()
	perms := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	log := &recordingCmdLog{}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, log)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "failing test",
		Command:     "echo '--- FAIL: TestX' && exit 1",
	})
	require.False(t, resp.IsError)

	require.Len(t, log.runs, 1)
	run := log.runs[0]
	require.Equal(t, "test-session", run.SessionID)
	require.Equal(t, "echo '--- FAIL: TestX' && exit 1", run.Command)
	require.Equal(t, workingDir, run.CWD)
	require.Equal(t, 1, run.ExitCode)
	require.True(t, run.Ran, "a real exit status means the command ran")
	require.False(t, run.Interrupted)
	require.Contains(t, run.Stdout, "FAIL: TestX")
}

func TestBashTool_RecordsComponents(t *testing.T) {
	workingDir := t.TempDir()
	perms := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	log := &recordingCmdLog{}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, log)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// The whole #185 chain in one pin: the pipe's clean final code
	// reaches the ledger alongside the component list that lets the
	// write path recover the laundered failure.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "piped failure",
		Command:     "sh -c 'exit 3' | cat",
	})
	require.False(t, resp.IsError)

	require.Len(t, log.runs, 1)
	run := log.runs[0]
	require.Equal(t, 0, run.ExitCode, "the composite's final code is head's clean close")
	require.True(t, run.Ran)
	var exits []int
	var shCall *shell.ComponentCall
	for i := range run.Components {
		exits = append(exits, run.Components[i].Exit)
		if len(run.Components[i].Args) > 0 && run.Components[i].Args[0] == "sh" {
			shCall = &run.Components[i]
		}
	}
	require.Contains(t, exits, 3,
		"the component log must carry the failed element's real verdict")
	require.NotNil(t, shCall)
	require.Equal(t, []string{"sh", "-c", "exit 3"}, shCall.Args)
	require.NotEmpty(t, shCall.Dir)
}

func TestBashTool_BlockedCommandNotAProjectFailure(t *testing.T) {
	workingDir := t.TempDir()
	perms := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	log := &recordingCmdLog{}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, log)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// The static check cannot resolve a command name built from a
	// variable, so it reports no Matched name and the run-time block
	// list stays armed; the interp then expands $c to sudo and the
	// block list denies it before execution.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "blocked",
		Command:     "c=sudo; $c echo hi",
	})

	// A policy denial never executed: it reaches the ledger (noted)
	// but carries Ran=false so no failure row opens.
	require.Len(t, log.runs, 1)
	run := log.runs[0]
	require.False(t, run.Ran, "a blocked command never ran")
	require.NotNil(t, run.Err)
	_ = resp
}

// The pre-exec HEAD stamp the tool takes must reach the recorded
// run — it's the seam that keeps inter-run external commits out of
// the run's attribution range (#365).
func TestBashTool_StampsPreRunHead(t *testing.T) {
	workingDir := t.TempDir()
	perms := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	log := &recordingCmdLog{headBefore: "pre-sha"}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, log)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "stamped run",
		Command:     "echo hi",
	})
	require.False(t, resp.IsError)
	require.Len(t, log.runs, 1)
	require.Equal(t, "pre-sha", log.runs[0].HeadBefore,
		"HeadSHA's stamp rides the shell into Run.HeadBefore")
}

// A background job outlives its tool call — the stamp rides
// BackgroundShell so whichever consumer records the completion
// carries it through (#365).
func TestJobOutput_CarriesPreRunHead(t *testing.T) {
	workingDir := t.TempDir()
	log := &recordingCmdLog{}
	tool := NewJobOutputTool(workingDir, log)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	bgManager := shell.GetBackgroundShellManager()
	bgShell, err := bgManager.Start(ctx, workingDir, nil, "echo done", "", config.DefaultMaxBackgroundJobs, "pre-sha")
	require.NoError(t, err)
	bgShell.Wait()
	defer func() { _ = bgManager.Remove(bgShell.ID) }()

	input, err := json.Marshal(JobOutputParams{ShellID: bgShell.ID})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: JobOutputToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Len(t, log.runs, 1)
	require.Equal(t, "pre-sha", log.runs[0].HeadBefore,
		"the job's stamped pre reaches the recorded run")
}

func TestResolveBlockedCommands(t *testing.T) {
	t.Parallel()

	t.Run("nil config uses the defaults", func(t *testing.T) {
		t.Parallel()
		require.ElementsMatch(t, defaultBlockedCommands, resolveBlockedCommands(nil))
	})

	t.Run("blocked_commands adds to the defaults", func(t *testing.T) {
		t.Parallel()
		got := resolveBlockedCommands(&config.Permissions{
			BlockedCommands: []string{"kubectl", "terraform"},
		})
		require.Contains(t, got, "kubectl")
		require.Contains(t, got, "terraform")
		require.Contains(t, got, "curl", "defaults must still apply")
	})

	t.Run("allowed_commands removes defaults", func(t *testing.T) {
		t.Parallel()
		got := resolveBlockedCommands(&config.Permissions{
			AllowedCommands: []string{"curl", "wget"},
		})
		require.NotContains(t, got, "curl")
		require.NotContains(t, got, "wget")
		require.Contains(t, got, "sudo", "unrelated defaults must stay")
	})

	t.Run("allowed_commands wins over blocked_commands", func(t *testing.T) {
		t.Parallel()
		got := resolveBlockedCommands(&config.Permissions{
			BlockedCommands: []string{"kubectl"},
			AllowedCommands: []string{"kubectl"},
		})
		require.NotContains(t, got, "kubectl")
	})

	t.Run("built-in order is preserved and duplicates collapsed", func(t *testing.T) {
		t.Parallel()
		got := resolveBlockedCommands(&config.Permissions{
			BlockedCommands: []string{"curl", "kubectl", "kubectl"},
		})
		// The list is embedded verbatim in the tool description, so the
		// defaults must keep their hand-grouped order and additions go last.
		require.Equal(t, defaultBlockedCommands, got[:len(defaultBlockedCommands)])
		require.Equal(t, []string{"kubectl"}, got[len(defaultBlockedCommands):],
			"additions append once; a duplicate of a default is dropped")
	})

	t.Run("unblocking a command stops it reading as dangerous", func(t *testing.T) {
		t.Parallel()
		blocked := resolveBlockedCommands(&config.Permissions{AllowedCommands: []string{"curl"}})
		require.False(t, shell.IsCommandBlocked("curl https://example.com", blockFuncs(blocked)))
		require.True(t, shell.IsCommandBlocked("sudo rm -rf /", blockFuncs(blocked)))
	})
}

// TestBashTool_ApprovedDangerousCommandRuns pins the rule that a command the
// user was warned about and approved anyway reaches the shell unguarded:
// re-blocking it would override the answer they just gave.
func TestBashTool_ApprovedDangerousCommandRuns(t *testing.T) {
	workingDir := t.TempDir()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeNormal,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// `ifconfig` is on the default dangerous list, so it is prompted for.
	// Approving it must not then be overridden by the exec-time block list.
	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "approved dangerous command",
		Command:     "ifconfig --help",
	})

	require.Equal(t, 1, perms.requestCount, "a dangerous command must be prompted for")
	require.Equal(t, "ifconfig", perms.lastDanger,
		"the prompt must name what tripped the check")
	require.NotContains(t, resp.Content, "is a dangerous command",
		"an approved command must not be re-blocked at exec time")
}

// TestBashTool_UnflaggedCommandKeepsBlockList is the other side of that rule.
// The static check expands commands with globbing off, so a glob hides the
// dangerous name from it and yolo mode auto-approves the call unseen. The block
// list stays on at exec time, where the glob has resolved and the real command
// name is finally visible.
func TestBashTool_UnflaggedCommandKeepsBlockList(t *testing.T) {
	workingDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "ifconfig"), []byte("#!/bin/sh\n"), 0o755))
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeYolo,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "indirect dangerous command",
		Command:     "ifconf?g --help",
	})

	require.Empty(t, perms.lastDanger, "the static check cannot see through a glob")
	require.Contains(t, resp.Content, "is a dangerous command")
}

// TestBashTool_SysadminModeSkipsBlockList pins sysadmin mode as the deliberate
// exception: it asks for nothing and blocks nothing.
func TestBashTool_SysadminModeSkipsBlockList(t *testing.T) {
	workingDir := t.TempDir()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeSysadmin,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "ifconfig"), []byte("#!/bin/sh\nexit 0\n"), 0o755))

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "indirect dangerous command",
		Command:     "ifconf?g --help",
	})

	require.NotContains(t, resp.Content, "is a dangerous command")
}

// TestBashTool_DeniedDangerousCommandDoesNotRun is the other half: denial at
// the permission layer stops the command, no block list required.
func TestBashTool_DeniedDangerousCommandDoesNotRun(t *testing.T) {
	workingDir := t.TempDir()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  false,
		mode:   permission.PermissionModeNormal,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "denied dangerous command",
		Command:     "ifconfig --help",
	})

	require.Equal(t, 1, perms.requestCount)
	require.True(t, resp.IsError)
}

// TestBashTool_SafeReadOnlyPathStillEnforcesBlockList pins the one branch that
// never asks: a safe-prefix command skips the permission request entirely, so
// the block list stays on there as a backstop.
func TestBashTool_SafeReadOnlyPathStillEnforcesBlockList(t *testing.T) {
	workingDir := t.TempDir()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeNormal,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "safe command",
		Command:     "pwd",
	})

	require.Zero(t, perms.requestCount, "safe read-only commands must not prompt")
	require.False(t, resp.IsError)
}

// TestBashTool_YoloModeDoesNotPromptForDangerousCommands pins the rule that
// yolo mode means stop asking. It drives the real permission service rather
// than a mock, because the whole behaviour under test lives there.
func TestBashTool_YoloModeDoesNotPromptForDangerousCommands(t *testing.T) {
	workingDir := t.TempDir()
	perms := permission.NewPermissionService(workingDir, nil, nil)
	perms.SetPermissionMode(permission.PermissionModeYolo)

	// Any prompt would land here. Nothing should.
	prompts := perms.Subscribe(t.Context())

	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	// `ifconfig` is on the default dangerous list. In yolo it must run
	// without asking, and without being re-blocked at exec time either.
	// Run it off the test goroutine so a regression that reinstates the
	// prompt fails here in a few seconds rather than hanging until the
	// whole package times out.
	content := make(chan string, 1)
	go func() {
		resp := runBashTool(t, tool, ctx, BashParams{
			Description: "dangerous command in yolo mode",
			Command:     "ifconfig --help",
		})
		content <- resp.Content
	}()

	var got string
	select {
	case got = <-content:
	case <-time.After(30 * time.Second):
		t.Fatal("yolo mode blocked waiting for a permission prompt that should never have been raised")
	}

	require.NotContains(t, got, "is a dangerous command",
		"a dangerous command must not be blocked in yolo mode")

	select {
	case p := <-prompts:
		t.Fatalf("yolo mode must not prompt, but got a request for %q", p.Payload.Description)
	default:
	}
}

// TestBashTool_NormalModeStillPromptsForDangerousCommands is the other half of
// the rule: manual mode keeps asking.
func TestBashTool_NormalModeStillPromptsForDangerousCommands(t *testing.T) {
	workingDir := t.TempDir()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeNormal,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	runBashTool(t, tool, ctx, BashParams{
		Description: "dangerous command in normal mode",
		Command:     "ifconfig --help",
	})

	require.Equal(t, 1, perms.requestCount, "normal mode must still ask")
	require.Equal(t, "ifconfig", perms.lastDanger,
		"the prompt must name what tripped the check")
}

// TestBlockedCommandErrorNamesTheCommand checks that a command stopped at
// exec time names the command that was recognised. The static check and the
// run-time check have to agree about that, or the same command is described
// two different ways depending on which one caught it.
func TestBlockedCommandErrorNamesTheCommand(t *testing.T) {
	workingDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "ifconfig"), []byte("#!/bin/sh\n"), 0o755))
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
		mode:   permission.PermissionModeYolo,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(nil, perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")

	resp := runBashTool(t, tool, ctx, BashParams{
		Description: "indirect dangerous command",
		Command:     "ifconf?g --help",
	})

	require.Contains(t, resp.Content, "ifconfig is a dangerous command",
		"the error must name the command, not the spelling that reached it")
}
