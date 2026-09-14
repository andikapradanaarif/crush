// Package index maintains a persistent, per-project map of source
// files — top-level symbols and file-to-file references — used by the
// map tool to answer "where does X live" without model-driven grep
// roundtrips. The index is a rebuildable cache in a sidecar database
// under the project data directory: deleting it is always safe, and
// every read path tolerates an empty or partially built store.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// IndexFilename is the sidecar database name inside the project data
// directory (typically <project>/.crush/).
const IndexFilename = "index.db"

// schema is the full v1 schema. The chunks/embeddings tables are
// reserved for the semantic (v2) layer — created now so adding it is
// an additive query mode, not a migration.
const schema = `
CREATE TABLE IF NOT EXISTS files (
	path       TEXT PRIMARY KEY,
	mtime      INTEGER NOT NULL,
	size       INTEGER NOT NULL,
	indexed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS symbols (
	path     TEXT NOT NULL,
	name     TEXT NOT NULL,
	kind     TEXT NOT NULL,
	line     INTEGER NOT NULL,
	exported INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_path ON symbols(path);
CREATE TABLE IF NOT EXISTS refs (
	src_path TEXT NOT NULL,
	dst_path TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS refs_dst ON refs(dst_path);
CREATE INDEX IF NOT EXISTS refs_src ON refs(src_path);
-- In-degree ranking assumes dedup'd (src, dst) pairs — pin it in the
-- schema, not just in the tagger's refSet convention.
CREATE UNIQUE INDEX IF NOT EXISTS refs_pair ON refs(src_path, dst_path);
CREATE TABLE IF NOT EXISTS chunks (
	id         INTEGER PRIMARY KEY,
	path       TEXT NOT NULL,
	start_line INTEGER NOT NULL,
	end_line   INTEGER NOT NULL,
	text_hash  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS embeddings (
	chunk_id INTEGER PRIMARY KEY,
	vector   BLOB,
	model_id TEXT
);
`

// Service is the project index handle. The underlying file is shared
// by every session — including subagent child sessions — running
// against the same project; use Shared to get the process-wide
// instance rather than opening one per caller.
type Service struct {
	dataDir string
	root    string

	once    sync.Once
	db      *sql.DB
	initErr error

	buildMu   sync.Mutex // guards buildDone swap and build state
	buildDone chan struct{}
	indexing  atomic.Bool
	buildErr  error
	lastBuild atomic.Int64 // unixnano of last completed walk

	lastDirtyScan atomic.Int64 // unixnano; bounds refreshDirty frequency
}

var shared sync.Map // (dataDir, workingDir) -> *Service

// byWorkingDir registers Shared services so file-mutating tools can
// notify the index of writes without holding a handle. A Service is
// registered only when constructed via Shared — services that were
// never built (project_index off) never register, so NotifyWritten
// is a no-op in that configuration.
var byWorkingDir sync.Map // workingDir -> *Service

func newService(dataDir, workingDir string) *Service {
	return &Service{dataDir: dataDir, root: workingDir, buildDone: make(chan struct{})}
}

// Shared returns the process-wide Service for a project, opening it
// lazily on first use. One handle per working dir keeps a long-
// running process from accumulating open connections to index.db as
// agents and sub-agents each build their toolsets. Handles returned
// here are process-lifetime and not closed.
func Shared(dataDir, workingDir string) *Service {
	key := dataDir + "\x00" + workingDir
	if v, ok := shared.Load(key); ok {
		return v.(*Service)
	}
	v, loaded := shared.LoadOrStore(key, newService(dataDir, workingDir))
	svc := v.(*Service)
	if !loaded {
		byWorkingDir.Store(workingDir, svc)
	}
	return svc
}

// Open creates a standalone Service — for tests and one-off callers.
// Production code should use Shared.
func Open(dataDir, workingDir string) (*Service, error) {
	if dataDir == "" || workingDir == "" {
		return nil, fmt.Errorf("index requires data dir and working dir")
	}
	s := newService(dataDir, workingDir)
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

// NotifyWritten tells the index that a write tool just mutated
// absPath — the path is re-tagged (or newly tagged) immediately, so
// files the agent creates or edits mid-session are visible without
// waiting for a re-walk. No-op for paths outside every registered
// project root and when no service exists (project_index disabled).
func NotifyWritten(absPath string) {
	byWorkingDir.Range(func(_, v any) bool {
		v.(*Service).touchFile(absPath)
		return true
	})
}

// touchFile re-tags absPath when it lives under this service's root.
// refreshIfStale handles both stale and never-indexed paths, so a
// newly created file lands in the index on its first write.
func (s *Service) touchFile(absPath string) {
	rel, err := filepath.Rel(s.root, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return // Outside this project — don't even open the DB.
	}
	if err := s.init(); err != nil {
		return
	}
	info, err := os.Stat(absPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.dropFile(context.Background(), filepath.ToSlash(rel))
		return
	}
	if err != nil {
		return // Transient stat failure — keep the row, stay stale.
	}
	s.refreshIfStale(context.Background(), filepath.ToSlash(rel),
		info.ModTime().UnixNano(), info.Size())
}

// Ready reports whether the index database could be opened and
// migrated — triggers the lazy open on first call.
func (s *Service) Ready() error {
	return s.init()
}

// init opens the sidecar database and creates the schema, once.
func (s *Service) init() error {
	s.once.Do(func() {
		if s.dataDir == "" || s.root == "" {
			// Shared skips Open's validation — an empty data dir
			// would otherwise write index.db into the process CWD,
			// dodging the .crush gitignore self-protection.
			s.initErr = fmt.Errorf("index requires data dir and working dir")
			return
		}
		conn, err := db.OpenDBFile(filepath.Join(s.dataDir, IndexFilename))
		if err != nil {
			s.initErr = err
			return
		}
		if _, err := conn.Exec(schema); err != nil {
			conn.Close()
			s.initErr = fmt.Errorf("failed to init index schema: %w", err)
			return
		}
		s.db = conn
	})
	return s.initErr
}

// Close releases the database handle. Not for Shared instances.
func (s *Service) Close() error {
	if err := s.init(); err != nil {
		return err
	}
	return s.db.Close()
}

// rewalkInterval is how long a completed build stays authoritative
// before the next query triggers a background re-walk. The walk is
// incremental (unchanged files are skipped by {mtime, size}), so a
// re-walk over a quiet tree costs only the stat pass.
const rewalkInterval = 5 * time.Minute

// failRetryInterval replaces rewalkInterval after a failed build —
// a transient walk error must not suppress retries for five minutes,
// but a persistent failure shouldn't be retried per query either.
const failRetryInterval = 30 * time.Second

// walkTimeout bounds a single build — a pathological tree or network
// filesystem must not hang the background walk forever; a timed-out
// build surfaces as buildErr in the skeleton header.
const walkTimeout = 10 * time.Minute

// ensureStarted starts a walk in the background when the index has
// never been built or the last build is older than rewalkInterval.
// Query paths serve whatever has committed so far — the walk writes
// in batches, so a map call mid-build returns the current index
// instead of blocking on the tree.
// buildInterval returns how long the last build stays authoritative
// — shorter after a failure so transient errors retry quickly.
// Caller supplies the last build's error; pass s.buildErr while
// holding buildMu or s.err() otherwise.
func buildInterval(err error) time.Duration {
	if err != nil {
		return failRetryInterval
	}
	return rewalkInterval
}

// err returns the last build's error under buildMu — a plain field
// read without the lock would be correct only via the atomic
// ordering on indexing/buildDone, which is easy to break in a
// future edit.
func (s *Service) err() error {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	return s.buildErr
}

func (s *Service) ensureStarted(ctx context.Context) {
	fresh := s.lastBuild.Load()
	if fresh != 0 && time.Since(time.Unix(0, fresh)) < buildInterval(s.err()) {
		return
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if s.indexing.Load() {
		return // A walk is already running.
	}
	if fresh != 0 && time.Since(time.Unix(0, fresh)) < buildInterval(s.buildErr) {
		return // Re-check under the lock.
	}
	done := make(chan struct{})
	s.buildDone = done
	s.indexing.Store(true)
	go func() {
		// Detached: the build must outlive the tool call that
		// triggered it; bounded so it cannot run forever.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), walkTimeout)
		defer cancel()
		err := s.walk(bctx)
		s.buildMu.Lock()
		s.buildErr = err
		s.buildMu.Unlock()
		s.lastBuild.Store(time.Now().UnixNano())
		s.indexing.Store(false)
		close(done)
	}()
}

// EnsureIndexed starts a build if needed and blocks until the
// current one completes. Tests and callers that need a complete
// index use this; the map tool's query paths only call
// ensureStarted.
func (s *Service) EnsureIndexed(ctx context.Context) error {
	if err := s.init(); err != nil {
		return err
	}
	s.ensureStarted(ctx)
	s.buildMu.Lock()
	done := s.buildDone
	s.buildMu.Unlock()
	select {
	case <-done:
		return s.err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// indexedFile returns the recorded {mtime, size} for a path; found is
// false when the file is unknown to the index.
func (s *Service) indexedFile(ctx context.Context, path string) (mtime, size int64, found bool) {
	err := s.db.QueryRowContext(ctx,
		`SELECT mtime, size FROM files WHERE path = ?`, path).Scan(&mtime, &size)
	if err != nil {
		return 0, 0, false
	}
	return mtime, size, true
}

// knownDir reports whether any indexed file sits under dir — the
// refresh path's equivalent of the walk's knownDirs set, needed for
// refs that resolve to directories (Go package imports). Uses a
// literal prefix test so metacharacters in dir can't act as a
// pattern.
func (s *Service) knownDir(ctx context.Context, dir string) bool {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM files WHERE instr(path, ?) = 1 LIMIT 1`, dir+"/").Scan(&n)
	return err == nil
}

// indexedAt returns the newest index timestamp — the build date the
// rendered map reports so its freshness is visible to the model.
func (s *Service) indexedAt(ctx context.Context) time.Time {
	var ts int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(indexed_at), 0) FROM files`).Scan(&ts); err != nil || ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// fileCount reports how many files the index holds — used by renderers
// to say whether the skeleton reflects a complete walk.
func (s *Service) fileCount(ctx context.Context) int {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// dropFile removes a path and all its derived rows.
func (s *Service) dropFile(ctx context.Context, path string) {
	for _, q := range []string{
		`DELETE FROM files WHERE path = ?`,
		`DELETE FROM symbols WHERE path = ?`,
		`DELETE FROM refs WHERE src_path = ?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, path); err != nil {
			slog.Debug("Index drop failed", "path", path, "error", err)
		}
	}
}
