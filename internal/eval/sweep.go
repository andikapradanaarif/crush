package eval

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleTempMaxAge is the age after which an eval-owned temp dir is
// treated as a leak. Live runs write into their home/workdir
// constantly (logs, session DB WAL, agent edits), so a fresh mtime is
// a reliable liveness signal — and the lock that calls the sweep
// already serializes real eval invocations.
const staleTempMaxAge = 24 * time.Hour

// staleTempMaxPerSweep bounds one invocation's cleanup — the chmod
// walk on read-only module trees is not cheap, and a huge backlog
// must not stall eval start. Repeated invocations drain the rest.
const staleTempMaxPerSweep = 64

// evalTempPrefixes are the temp namespaces this harness owns:
// crush-eval-home-* (pinned run homes) and eval-run-* (materialized
// workdirs, including their .crush-data siblings). go-build* is
// deliberately excluded — that namespace belongs to every go build on
// the machine, not just eval children.
var evalTempPrefixes = []string{"crush-eval-home-", "eval-run-"}

// sweepStaleTemps garbage-collects eval-owned temp dirs under the
// temp root and the configured workdir parent. It is the backstop for
// homes and workdirs leaked by killed or hung runs: Close only runs
// on graceful exits and os.MkdirTemp registers no cleanup. Called
// under the eval lock so no live eval's dirs can be collected.
// Best-effort — failures are logged and skipped, never fatal.
func (r *Runner) sweepStaleTemps() {
	now := time.Now()
	parents := map[string]bool{os.TempDir(): true, r.workParent(): true}
	for parent := range parents {
		if n := sweepStaleTemps(parent, now); n > 0 {
			slog.Info("Swept stale eval temp dirs", "dir", parent, "count", n)
		}
	}
}

// sweepStaleTemps removes eval-owned dirs under parent whose mtime
// predates the age threshold, returning the count removed.
func sweepStaleTemps(parent string, now time.Time) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if removed >= staleTempMaxPerSweep {
			break
		}
		if !e.IsDir() || !isEvalTempDir(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < staleTempMaxAge {
			continue
		}
		path := filepath.Join(parent, e.Name())
		chmodTreeWritable(path)
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("Failed to remove stale eval temp dir", "path", path, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// isEvalTempDir reports whether name belongs to an eval-owned temp
// namespace.
func isEvalTempDir(name string) bool {
	for _, p := range evalTempPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// chmodTreeWritable marks every directory in the tree user-writable.
// Leaked homes carry go/pkg/mod trees written read-only — unlink only
// needs writable parent directories, so chmod-ing dirs is enough.
func chmodTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}
