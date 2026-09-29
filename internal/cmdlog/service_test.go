package cmdlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx        context.Context
	svc        Service
	workingDir string
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	workingDir := t.TempDir()
	return &testEnv{
		ctx:        t.Context(),
		svc:        NewService(db.New(conn), workingDir),
		workingDir: workingDir,
	}
}

// run simulates a completed run that reached a real exit status —
// Ran mirrors what bash.go computes with shell.IsExitStatus.
func run(env *testEnv, sessionID, command, cwd, stdout, stderr string, runErr error, exitCode int) {
	env.svc.RecordRun(env.ctx, Run{
		SessionID: sessionID,
		Command:   command,
		CWD:       cwd,
		Stdout:    stdout,
		Stderr:    stderr,
		Err:       runErr,
		ExitCode:  exitCode,
		Ran:       true,
	})
}

// touch creates a workspace file so the os.Stat gate in extractFiles
// accepts the hint.
func touch(t *testing.T, env *testEnv, rel string) {
	t.Helper()
	path := filepath.Join(env.workingDir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
}

func TestRecordRun_UpsertsCommandLedger(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "go  test   ./...", env.workingDir, "", "", nil, 0)
	run(env, "s2", "go test ./...", env.workingDir, "", "", nil, 1)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)

	cmd := cmds[0]
	// Cosmetic whitespace respelling shares one normalized row.
	require.Equal(t, "go test ./...", cmd.CmdNorm)
	require.Equal(t, "test", cmd.Kind)
	require.Equal(t, int64(1), cmd.LastExit)
	require.Equal(t, int64(1), cmd.OKCount)
	require.Equal(t, int64(1), cmd.FailCount)
	require.Equal(t, "s2", cmd.LastSessionID)
}

func TestRecordRun_CommandKinds(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "go build ./...", env.workingDir, "", "", nil, 0)
	run(env, "s1", "npm run lint", env.workingDir, "", "", nil, 0)
	run(env, "s1", "npm run dev", env.workingDir, "", "", nil, 0)
	run(env, "s1", "ls -la", env.workingDir, "", "", nil, 0)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	kinds := map[string]string{}
	for _, c := range cmds {
		kinds[c.CmdNorm] = c.Kind
	}
	require.Equal(t, "build", kinds["go build ./..."])
	require.Equal(t, "lint", kinds["npm run lint"])
	require.Equal(t, "run", kinds["npm run dev"])
	require.Equal(t, "other", kinds["ls -la"])
}

func TestRecordRun_FailureLifecycle(t *testing.T) {
	env := setupTest(t)
	touch(t, env, "foo_test.go")
	stderr := "FAIL: TestFoo\n\tfoo_test.go:42: expected 1, got 2"

	run(env, "s1", "go test ./...", env.workingDir, "", stderr, nil, 1)

	failures, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Equal(t, "go test ./...", failures[0].Cmd)
	require.Equal(t, ".", failures[0].CWD)
	require.Equal(t, "FAIL: TestFoo", failures[0].Headline)
	require.Contains(t, failures[0].Files, "foo_test.go")
	require.Empty(t, failures[0].ResolvedIn)

	// A clean run of the same normalized command in the same
	// directory resolves it in the resolving session.
	run(env, "s2", "go  test ./...", env.workingDir, "", "", nil, 0)
	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)
}

func TestRecordRun_StdoutFailure(t *testing.T) {
	env := setupTest(t)
	touch(t, env, "parse_test.go")
	// go test writes FAIL lines to stdout with empty stderr.
	stdout := "ok  \texample.com/pkg/a\t0.012s\n--- FAIL: TestParse (0.00s)\n    parse_test.go:31: bad parse\nFAIL\texample.com/pkg/b\t0.041s"

	run(env, "s1", "go test ./...", env.workingDir, stdout, "", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	// The verdict line headlines, not the "ok pkg" prelude or the
	// bare exec error.
	require.Equal(t, "--- FAIL: TestParse (0.00s)", open[0].Headline)
	require.Contains(t, open[0].Files, "parse_test.go")
	// Hostnames and module paths parse as dotted tokens but are not
	// files — the on-disk gate drops them.
	for _, f := range open[0].Files {
		require.NotContains(t, f, "example.com")
	}
}

func TestRecordRun_DistinctStdoutFailures(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "go test ./...", env.workingDir, "--- FAIL: TestA\n", "", nil, 1)
	run(env, "s2", "go test ./...", env.workingDir, "--- FAIL: TestB\n", "", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	// Two distinct test failures stay two rows, not one collapsed
	// "exit status 1".
	require.Len(t, open, 2)
}

func TestRecordRun_DirectoryScopedResolution(t *testing.T) {
	env := setupTest(t)
	api := filepath.Join(env.workingDir, "packages", "api")
	web := filepath.Join(env.workingDir, "packages", "web")
	touch(t, env, filepath.Join("packages", "api", "auth.spec.ts"))
	touch(t, env, filepath.Join("packages", "web", "cart.spec.ts"))

	run(env, "s1", "npm test", api, "", "FAIL auth.spec.ts", nil, 1)
	run(env, "s1", "npm test", web, "", "FAIL cart.spec.ts", nil, 1)
	// Green in web must not close api's open row.
	run(env, "s2", "npm test", web, "", "", nil, 0)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, filepath.Join("packages", "api"), open[0].CWD)

	// The ledger scopes the same way: "npm test" in api and web are
	// two rows, so "run the tests" knows where it last ran.
	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 2)
	cwds := map[string]bool{}
	for _, c := range cmds {
		require.Equal(t, "npm test", c.CmdNorm)
		cwds[c.CWD] = true
	}
	require.True(t, cwds[filepath.Join("packages", "api")])
	require.True(t, cwds[filepath.Join("packages", "web")])
}

func TestRecordRun_RefailReopensAndRefreshes(t *testing.T) {
	env := setupTest(t)
	touch(t, env, "foo_test.go")

	run(env, "s1", "go test ./...", env.workingDir, "", "FAIL: TestFoo\n\tfoo_test.go:42: boom", nil, 1)
	run(env, "s2", "go test ./...", env.workingDir, "", "", nil, 0)
	run(env, "s3", "go test ./...", env.workingDir, "", "FAIL: TestFoo\n\tfoo_test.go:88: new hint", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	// The re-fail refreshes the observation: the latest line hint
	// shows, not the stale first sighting.
	require.Contains(t, open[0].Headline, "FAIL: TestFoo")
	require.Contains(t, open[0].Files, "foo_test.go")
}

func TestRecordRun_SignatureIgnoresLineNumbers(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "go test ./...", env.workingDir, "", "FAIL: TestFoo\n\tfoo_test.go:42: boom", nil, 1)
	run(env, "s1", "go test ./...", env.workingDir, "", "FAIL: TestFoo\n\tfoo_test.go:97: boom", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	// Same test, different line number — one signature, not two.
	require.Len(t, open, 1)
}

func TestRecordRun_InterruptedSkipsFailure(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, Run{
		SessionID:   "s1",
		Command:     "go test ./...",
		CWD:         env.workingDir,
		ExitCode:    130,
		Interrupted: true,
	})

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)

	// The ledger still notes the run — neither ok nor fail counted,
	// and last_exit keeps any prior verdict rather than the
	// interrupt code.
	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, int64(0), cmds[0].OKCount)
	require.Equal(t, int64(0), cmds[0].FailCount)
}

func TestRecordRun_SigintExitWithoutFlagCountsAsInterrupt(t *testing.T) {
	env := setupTest(t)

	// A real SIGINT exit (130) that didn't arrive via the ctx path
	// is still not a project failure.
	env.svc.RecordRun(env.ctx, Run{
		SessionID: "s1",
		Command:   "go test ./...",
		CWD:       env.workingDir,
		ExitCode:  130,
	})

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)
}

func TestRecordRun_InterruptKeepsPriorVerdict(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "make", env.workingDir, "", "", nil, 1)
	env.svc.RecordRun(env.ctx, Run{
		SessionID:   "s1",
		Command:     "make",
		CWD:         env.workingDir,
		ExitCode:    130,
		Interrupted: true,
	})

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	// The interrupt noted the run but the last real verdict stands.
	require.Equal(t, int64(1), cmds[0].LastExit)
}

func TestRecordRun_HeadlineRedacted(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "./deploy.sh", env.workingDir, "", "auth failed: API_KEY=supersecretvalue999", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.NotContains(t, open[0].Headline, "supersecretvalue999")
}

func TestRecordRun_CommandRedacted(t *testing.T) {
	env := setupTest(t)

	// Credentials inline in the command itself get scrubbed before
	// the durable row — joins stay deterministic on the redacted key.
	run(env, "s1", `curl -H "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.secret" https://x`, env.workingDir, "", "", nil, 0)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.NotContains(t, cmds[0].CmdNorm, "eyJhbGciOiJIUzI1NiJ9.secret")
	require.Contains(t, cmds[0].CmdNorm, "[REDACTED]")
}

func TestRecordRun_HeadlineFallsBackToError(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "make build", env.workingDir, "", "", errors.New("exit status 2"), 2)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, "exit status 2", open[0].Headline)
}

func TestListCommands_MostRecentFirst(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "make", env.workingDir, "", "", nil, 0)
	time.Sleep(10 * time.Millisecond)
	run(env, "s1", "go vet ./...", env.workingDir, "", "", nil, 0)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 2)
	require.Equal(t, "go vet ./...", cmds[0].CmdNorm)
	require.Equal(t, "lint", cmds[0].Kind)
}

func TestRecordRun_EmptyCommandSkipped(t *testing.T) {
	env := setupTest(t)

	run(env, "s1", "   ", env.workingDir, "", "", nil, 0)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, cmds)
}

func TestRecordRun_FilesWorkspaceRelative(t *testing.T) {
	env := setupTest(t)
	api := filepath.Join(env.workingDir, "packages", "api")
	touch(t, env, filepath.Join("packages", "api", "auth.spec.ts"))
	touch(t, env, filepath.Join("packages", "api", "errors.go"))

	// A path relative to the run's cwd joins as workspace-relative —
	// the spelling file_heat carries — while dotted identifiers like
	// errors.New are not files, and paths escaping the root drop.
	run(env, "s1", "npm test", api, "", "FAIL auth.spec.ts\n  at errors.New (errors.go:1)\n  ../../etc/passwd:3 leaked", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Contains(t, open[0].Files, filepath.Join("packages", "api", "auth.spec.ts"))
	for _, f := range open[0].Files {
		// The identifier errors.New is not a file; errors.go inside
		// the stack frame is.
		require.NotEqual(t, "errors.New", filepath.Base(f))
		require.NotContains(t, f, "passwd")
	}
}

func TestRecordRun_DeniedIsNotAFailure(t *testing.T) {
	env := setupTest(t)

	// A blockHandler denial never executed: the ledger notes the
	// attempt without a verdict and no failure row opens.
	env.svc.RecordRun(env.ctx, Run{
		SessionID: "s1",
		Command:   "sudo rm -rf /",
		CWD:       env.workingDir,
		Err:       errors.New(`command is not allowed for security reasons: "sudo"`),
		ExitCode:  1,
		Ran:       false,
	})

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, int64(-1), cmds[0].LastExit)
	require.Equal(t, int64(0), cmds[0].OKCount)
	require.Equal(t, int64(0), cmds[0].FailCount)
}

func TestRecordRun_FilesScannedRedacted(t *testing.T) {
	env := setupTest(t)
	// The credential-shaped token would resolve to a real workspace
	// file — it persists only if extraction skipped redaction.
	touch(t, env, "abcdefgh1234.pem")

	run(env, "s1", "go test ./...", env.workingDir, "",
		"FAIL: TestAuth\nAuthorization: Bearer abcdefgh1234.pem", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	for _, f := range open[0].Files {
		require.NotContains(t, f, "pem")
	}
}

func TestRecordRun_GoBuildDiagnosticHeadline(t *testing.T) {
	env := setupTest(t)

	// go build errors carry no "error" word — the file:line:col:
	// diagnostic is the verdict line, not the "# pkg" banner.
	run(env, "s1", "go build ./...", env.workingDir, "",
		"# command-line-arguments\nmain.go:10:2: undefined: doThing", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, "main.go:10:2: undefined: doThing", open[0].Headline)
}

func TestRecordRun_StdoutSurvivesLoudStderr(t *testing.T) {
	env := setupTest(t)
	touch(t, env, "late_test.go")

	// Each stream scans its own first lines — a loud stderr can't
	// starve stdout's file tokens.
	loud := ""
	for i := range 50 {
		loud += fmt.Sprintf("noise line %d\n", i)
	}
	run(env, "s1", "go test ./...", env.workingDir, "FAIL late_test.go:9", loud, nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Contains(t, open[0].Files, "late_test.go")
}

func TestRecordRun_StderrNoiseDoesNotShadowStdoutVerdict(t *testing.T) {
	env := setupTest(t)

	// A constant stderr warning must not headline: stdout's verdict
	// line wins, so two distinct test failures stay two rows.
	run(env, "s1", "npm test", env.workingDir, "--- FAIL: TestA", "npm warn deprecated foo", nil, 1)
	run(env, "s2", "npm test", env.workingDir, "--- FAIL: TestB", "npm warn deprecated foo", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 2)
	for _, f := range open {
		require.Contains(t, f.Headline, "FAIL")
		require.NotContains(t, f.Headline, "npm warn")
	}
}

func TestRecordRun_NumberedTestsStayDistinct(t *testing.T) {
	env := setupTest(t)

	// Digits in identifiers are identity: TestParse2 and TestParse3
	// are different failures, even though :line noise still joins
	// the same test across line moves.
	run(env, "s1", "go test ./...", env.workingDir, "--- FAIL: TestParse2 (0.01s)", "", nil, 1)
	run(env, "s1", "go test ./...", env.workingDir, "--- FAIL: TestParse3 (0.02s)", "", nil, 1)
	run(env, "s1", "go test ./...", env.workingDir, "--- FAIL: TestParse2 (0.03s)", "", nil, 1)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 2)
}
