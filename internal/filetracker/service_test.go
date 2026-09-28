package filetracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	ctx        context.Context
	q          *db.Queries
	svc        Service
	workingDir string
}

func setupTest(t *testing.T) *testEnv {
	t.Helper()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	workingDir := t.TempDir()
	q := db.New(conn)
	return &testEnv{
		ctx:        t.Context(),
		q:          q,
		svc:        NewService(q, workingDir),
		workingDir: workingDir,
	}
}

func (e *testEnv) createSession(t *testing.T, sessionID string) {
	t.Helper()
	_, err := e.q.CreateSession(e.ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: "Test Session",
	})
	require.NoError(t, err)
}

func TestService_RecordRead(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-1"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)

	lastRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, lastRead.IsZero(), "expected non-zero time after recording read")
	require.WithinDuration(t, time.Now(), lastRead, 2*time.Second)
}

func TestService_LastReadTime_NotFound(t *testing.T) {
	env := setupTest(t)

	lastRead := env.svc.LastReadTime(env.ctx, "nonexistent-session", "/nonexistent/path")
	require.True(t, lastRead.IsZero(), "expected zero time for unread file")
}

func TestService_RecordRead_UpdatesTimestamp(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-2"
	path := "/path/to/file.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)
	firstRead := env.svc.LastReadTime(env.ctx, sessionID, path)
	require.False(t, firstRead.IsZero())

	synctest.Test(t, func(t *testing.T) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		env.svc.RecordRead(env.ctx, sessionID, path)
		secondRead := env.svc.LastReadTime(env.ctx, sessionID, path)

		require.False(t, secondRead.Before(firstRead), "second read time should not be before first")
	})
}

func TestService_RecordRead_DifferentSessions(t *testing.T) {
	env := setupTest(t)

	path := "/shared/file.go"
	session1, session2 := "session-1", "session-2"
	env.createSession(t, session1)
	env.createSession(t, session2)

	env.svc.RecordRead(env.ctx, session1, path)

	lastRead1 := env.svc.LastReadTime(env.ctx, session1, path)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, session2, path)
	require.True(t, lastRead2.IsZero(), "session 2 should not see session 1's read")
}

func TestService_RecordRead_DifferentPaths(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-3"
	path1, path2 := "/path/to/file1.go", "/path/to/file2.go"
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path1)

	lastRead1 := env.svc.LastReadTime(env.ctx, sessionID, path1)
	require.False(t, lastRead1.IsZero())

	lastRead2 := env.svc.LastReadTime(env.ctx, sessionID, path2)
	require.True(t, lastRead2.IsZero(), "path2 should not be recorded")
}

func TestService_RecordRead_StoresWorkspaceRelativePath(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-rel"
	path := filepath.Join(env.workingDir, "sub", "file.go")
	env.createSession(t, sessionID)

	env.svc.RecordRead(env.ctx, sessionID, path)

	readFiles, err := env.q.ListSessionReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, readFiles, 1)
	require.Equal(t, filepath.Join("sub", "file.go"), readFiles[0].Path)
}

func TestService_RecordRead_KeysStableAcrossCwd(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-cwd"
	path := filepath.Join(env.workingDir, "file.go")
	env.createSession(t, sessionID)

	// Recording and lookup happen under a cwd unrelated to the
	// workspace root; the stored key must not move.
	t.Chdir(t.TempDir())

	env.svc.RecordRead(env.ctx, sessionID, path)
	require.False(t, env.svc.LastReadTime(env.ctx, sessionID, path).IsZero())

	paths, err := env.svc.ListReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{filepathext.Canonical(path)}, paths)
}

func TestService_SymlinkedRootKeysConverge(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))

	// The service roots at the symlinked spelling while callers report
	// the canonical spelling — the LSP caller family's shape.
	svc := NewService(q, link)

	ctx := t.Context()
	sessionID := "test-session-symlink"
	_, err = q.CreateSession(ctx, db.CreateSessionParams{ID: sessionID, Title: "t"})
	require.NoError(t, err)

	svc.RecordRead(ctx, sessionID, filepath.Join(real, "f.go"))

	readFiles, err := q.ListSessionReadFiles(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, readFiles, 1)
	require.Equal(t, "f.go", readFiles[0].Path)

	paths, err := svc.ListReadFiles(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(filepathext.Canonical(real), "f.go")}, paths)
}

func TestService_RelativeWorkingDirResolvesAtConstruction(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	parent := t.TempDir()
	t.Chdir(parent)
	svc := NewService(db.New(conn), "ws")

	ctx := t.Context()
	sessionID := "test-session-relroot"
	_, err = db.New(conn).CreateSession(ctx, db.CreateSessionParams{
		ID:    sessionID,
		Title: "Test Session",
	})
	require.NoError(t, err)

	path := filepath.Join(parent, "ws", "file.go")
	svc.RecordRead(ctx, sessionID, path)
	require.False(t, svc.LastReadTime(ctx, sessionID, path).IsZero())

	paths, err := svc.ListReadFiles(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{filepathext.Canonical(path)}, paths)
}

func TestService_RecordRead_RelativeInputResolvesToWorkspace(t *testing.T) {
	env := setupTest(t)

	sessionID := "test-session-relin"
	env.createSession(t, sessionID)

	t.Chdir(t.TempDir())

	env.svc.RecordRead(env.ctx, sessionID, "sub/file.go")

	readFiles, err := env.q.ListSessionReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, readFiles, 1)
	require.Equal(t, filepath.Join("sub", "file.go"), readFiles[0].Path)

	paths, err := env.svc.ListReadFiles(env.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(filepathext.Canonical(env.workingDir), "sub", "file.go")}, paths)
}

func TestService_ListHotFiles(t *testing.T) {
	env := setupTest(t)

	env.createSession(t, "s1")
	env.createSession(t, "s2")
	env.createSession(t, "s3")

	env.svc.RecordRead(env.ctx, "s1", "shared.go")
	env.svc.RecordRead(env.ctx, "s1", "a.go")
	env.svc.RecordRead(env.ctx, "s2", "shared.go")
	env.svc.RecordRead(env.ctx, "s2", "b.go")
	env.svc.RecordRead(env.ctx, "s3", "current.go")

	root := filepathext.Canonical(env.workingDir)
	hot, err := env.svc.ListHotFiles(env.ctx, "s3", 10)
	require.NoError(t, err)
	require.Len(t, hot, 3)

	// Persistence outranks recency: the file two sessions read first.
	require.Equal(t, filepath.Join(root, "shared.go"), hot[0].Path)
	require.Equal(t, int64(2), hot[0].Sessions)
	require.False(t, hot[0].LastRead.IsZero())

	// The current session's reads are the working set, not heat.
	for _, h := range hot {
		require.NotEqual(t, "current.go", filepath.Base(h.Path))
	}
	require.ElementsMatch(t,
		[]string{filepath.Join(root, "a.go"), filepath.Join(root, "b.go")},
		[]string{hot[1].Path, hot[2].Path})

	// The cap applies at the query.
	hot, err = env.svc.ListHotFiles(env.ctx, "s3", 1)
	require.NoError(t, err)
	require.Len(t, hot, 1)
}
