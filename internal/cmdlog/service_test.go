package cmdlog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx context.Context
	svc Service
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return &testEnv{
		ctx: t.Context(),
		svc: NewService(db.New(conn)),
	}
}

func TestRecordRun_UpsertsCommandLedger(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "go  test   ./...", "/w", "", nil, 0, false)
	env.svc.RecordRun(env.ctx, "s2", "go test ./...", "/w", "", nil, 1, false)

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

	env.svc.RecordRun(env.ctx, "s1", "go build ./...", "/w", "", nil, 0, false)
	env.svc.RecordRun(env.ctx, "s1", "npm run lint", "/w", "", nil, 0, false)
	env.svc.RecordRun(env.ctx, "s1", "npm run dev", "/w", "", nil, 0, false)
	env.svc.RecordRun(env.ctx, "s1", "ls -la", "/w", "", nil, 0, false)

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
	stderr := "FAIL: TestFoo\n\tfoo_test.go:42: expected 1, got 2"

	env.svc.RecordRun(env.ctx, "s1", "go test ./...", "/w", stderr, nil, 1, false)

	failures, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Equal(t, "go test ./...", failures[0].Cmd)
	require.Equal(t, "FAIL: TestFoo", failures[0].Headline)
	require.Contains(t, failures[0].Files, "foo_test.go")
	require.Empty(t, failures[0].ResolvedIn)

	// A clean run of the same normalized command resolves it in the
	// resolving session.
	env.svc.RecordRun(env.ctx, "s2", "go  test ./...", "/w", "", nil, 0, false)
	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)
}

func TestRecordRun_RefailReopens(t *testing.T) {
	env := setupTest(t)
	stderr := "FAIL: TestFoo\n\tfoo_test.go:42: expected 1, got 2"

	env.svc.RecordRun(env.ctx, "s1", "go test ./...", "/w", stderr, nil, 1, false)
	env.svc.RecordRun(env.ctx, "s2", "go test ./...", "/w", "", nil, 0, false)
	env.svc.RecordRun(env.ctx, "s3", "go test ./...", "/w", stderr, nil, 1, false)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
}

func TestRecordRun_SignatureIgnoresLineNumbers(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "go test ./...", "/w", "FAIL: TestFoo\n\tfoo_test.go:42: boom", nil, 1, false)
	env.svc.RecordRun(env.ctx, "s1", "go test ./...", "/w", "FAIL: TestFoo\n\tfoo_test.go:97: boom", nil, 1, false)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	// Same test, different line number — one signature, not two.
	require.Len(t, open, 1)
}

func TestRecordRun_InterruptedSkipsFailure(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "go test ./...", "/w", "", nil, 130, true)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, open)

	// The ledger still notes the run — neither ok nor fail counted.
	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, int64(0), cmds[0].OKCount)
	require.Equal(t, int64(0), cmds[0].FailCount)
}

func TestRecordRun_HeadlineRedacted(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "./deploy.sh", "/w", "auth failed: API_KEY=supersecretvalue999", nil, 1, false)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.NotContains(t, open[0].Headline, "supersecretvalue999")
}

func TestRecordRun_HeadlineFallsBackToError(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "make build", "/w", "", errors.New("exit status 2"), 2, false)

	open, err := env.svc.ListOpenFailures(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, "exit status 2", open[0].Headline)
}

func TestListCommands_MostRecentFirst(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "make", "/w", "", nil, 0, false)
	time.Sleep(10 * time.Millisecond)
	env.svc.RecordRun(env.ctx, "s1", "go vet ./...", "/w", "", nil, 0, false)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Len(t, cmds, 2)
	require.Equal(t, "go vet ./...", cmds[0].CmdNorm)
	require.Equal(t, "lint", cmds[0].Kind)
}

func TestRecordRun_EmptyCommandSkipped(t *testing.T) {
	env := setupTest(t)

	env.svc.RecordRun(env.ctx, "s1", "   ", "/w", "", nil, 0, false)

	cmds, err := env.svc.ListCommands(env.ctx, 10)
	require.NoError(t, err)
	require.Empty(t, cmds)
}
