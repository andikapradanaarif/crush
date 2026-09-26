package eval

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// staleTempMaxAge is the age after which an eval-owned temp dir is
// treated as a leak. It applies only to dirs without a live PID
// marker — legacy names from pre-marker leaks, and Windows where
// pidAlive cannot probe. Top-level mtime is conservative on purpose:
// nested writes don't freshen it, so a live run's home can look idle
// for hours; 24h keeps even multi-day experiments safe.
const staleTempMaxAge = 24 * time.Hour

// staleTempMaxPerSweep bounds one invocation's cleanup across all
// parents — the chmod walk on read-only module trees is not cheap,
// and a huge backlog must not stall eval start. Repeated invocations
// drain the rest.
const staleTempMaxPerSweep = 64

// evalTempPrefixes are the temp namespaces this harness owns:
// crush-eval-home-* (pinned run homes) and eval-run-* (materialized
// workdirs, including their .crush-data siblings). go-build* is
// deliberately excluded — that namespace belongs to every go build on
// the machine, not just eval children. The prefixes are generic enough
// that a foreign tool could collide — accepted risk; the embedded PID
// marker narrows collateral to dirs with a parseable dead owner.
var evalTempPrefixes = []string{"crush-eval-home-", "eval-run-"}

// tempDirPattern prefixes a MkdirTemp pattern with the creating
// process's PID: "eval-run-*" becomes "eval-run-p<pid>-*". The sweep
// reads the marker back — a live owner is skipped regardless of age
// (the eval lock only serializes one EvalDir; a concurrent run under
// a different EvalDir could otherwise see a long-lived home go stale
// mid-run), and a dead owner is collectible immediately rather than
// after the age threshold.
func tempDirPattern(prefix string) string {
	return prefix + "p" + strconv.Itoa(os.Getpid()) + "-*"
}

// tempDirPID extracts the owner PID encoded by tempDirPattern.
// Pre-marker leaks and foreign collisions carry no "-p<pid>-" segment
// and return false — they fall through to the mtime rule.
func tempDirPID(name string) (int, bool) {
	for _, p := range evalTempPrefixes {
		rest, ok := strings.CutPrefix(name, p)
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(rest, "p")
		if !ok {
			return 0, false
		}
		num, _, ok := strings.Cut(rest, "-")
		if !ok {
			return 0, false
		}
		pid, err := strconv.Atoi(num)
		return pid, err == nil
	}
	return 0, false
}

// sweepStaleTemps garbage-collects eval-owned temp dirs under the
// temp root and the configured workdir parent. It is the backstop for
// homes and workdirs leaked by killed or hung runs: Close only runs
// on graceful exits and os.MkdirTemp registers no cleanup. Called
// under the eval lock. Best-effort — failures are logged and skipped,
// never fatal.
func (r *Runner) sweepStaleTemps() {
	now := time.Now()
	remaining := staleTempMaxPerSweep
	parents := map[string]bool{os.TempDir(): true, r.workParent(): true}
	for parent := range parents {
		if remaining <= 0 {
			break
		}
		n := sweepStaleTempsIn(parent, now, remaining)
		remaining -= n
		if n > 0 {
			slog.Info("Swept stale eval temp dirs", "dir", parent, "count", n)
		}
	}
}

// sweepStaleTempsIn removes eval-owned dirs under parent, at most
// maxRemove of them, returning the count removed.
func sweepStaleTempsIn(parent string, now time.Time, maxRemove int) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if removed >= maxRemove {
			break
		}
		if !e.IsDir() || !isEvalTempDir(e.Name()) {
			continue
		}
		if pid, marked := tempDirPID(e.Name()); marked && runtime.GOOS != "windows" {
			// A name-encoded PID is definitive liveness: alive owner
			// is skipped regardless of age, dead owner was orphaned
			// the moment its process exited — no age wait.
			if pidAlive(pid) {
				continue
			}
		} else {
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < staleTempMaxAge {
				continue
			}
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
// namespace. Symlinked dirs return false — DirEntry.IsDir doesn't
// follow links, and neither does the removal path.
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
