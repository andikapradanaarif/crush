package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// skeletonTopFiles caps how many high-centrality files the skeleton
// expands with their symbol lists.
const skeletonTopFiles = 15

// symDef is a symbol's definition site: file plus the extracted tag.
type symDef struct {
	path string
	tag  tag
}

// Skeleton renders the ranked project overview: directory layout with
// file counts, then the most-referenced files with their exported
// symbols. maxTokens bounds the output at ~4 bytes/token.
func (s *Service) Skeleton(ctx context.Context, maxTokens int) (string, error) {
	if err := s.init(); err != nil {
		return "", err
	}
	s.ensureStarted(ctx)
	var b strings.Builder
	stamp := "never"
	if ts := s.indexedAt(ctx); !ts.IsZero() {
		stamp = ts.Format(time.RFC3339)
	}
	switch {
	case s.indexing.Load():
		fmt.Fprintf(&b, "Project map — indexing in progress, %d files so far (partial; call again shortly for the full map)\n", s.fileCount(ctx))
	case s.err() != nil:
		fmt.Fprintf(&b, "Project map — index build failed: %v. Fall back to grep/glob.\n", s.err())
	default:
		fmt.Fprintf(&b, "Project map — %d files indexed (built %s)\n", s.fileCount(ctx), stamp)
	}

	if err := s.renderDirTree(ctx, &b, maxChars(maxTokens)/2); err != nil {
		return "", err
	}
	if err := s.renderTopFiles(ctx, &b, skeletonTopFiles); err != nil {
		return "", err
	}
	return capOutput(b.String(), maxTokens), nil
}

// Subtree renders every indexed file under relPath with its symbols.
// "." and "/" address the project root — every indexed file.
func (s *Service) Subtree(ctx context.Context, relPath string, maxTokens int) (string, error) {
	if err := s.init(); err != nil {
		return "", err
	}
	s.ensureStarted(ctx)
	if filepath.IsAbs(relPath) && relPath != "/" {
		return fmt.Sprintf("Path %q is absolute — pass a project-relative directory (e.g. \"internal/agent\"), or \".\" for the root.", relPath), nil
	}
	relPath = strings.Trim(filepath.ToSlash(filepath.Clean(relPath)), "/")
	if relPath == ".." || strings.HasPrefix(relPath, "../") {
		return fmt.Sprintf("Path %q escapes the project root — the index covers project-relative paths only.", relPath), nil
	}

	// LIKE metacharacters in the path are escaped so the input stays a
	// literal prefix.
	where := `path = ? OR path LIKE ? ESCAPE '\'`
	args := []any{relPath, escapeLike(relPath) + "/%"}
	if relPath == "" || relPath == "." {
		where, args, relPath = "1 = 1", nil, "."
	}

	// Refresh candidate paths BEFORE reading their symbols — re-tagging
	// after the rows are materialized would render pre-refresh data.
	pathRows, err := s.db.QueryContext(ctx,
		`SELECT path FROM files WHERE `+where+` ORDER BY path`, args...)
	if err != nil {
		return "", err
	}
	var paths []string
	for pathRows.Next() {
		var p string
		if err := pathRows.Scan(&p); err != nil {
			pathRows.Close()
			return "", err
		}
		paths = append(paths, p)
	}
	pathRows.Close()
	if len(paths) == 0 {
		return fmt.Sprintf("No indexed files under %q.", relPath), nil
	}
	s.refreshPaths(ctx, paths)

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.path, s.name, s.kind, s.line, s.exported
		FROM symbols s
		WHERE `+where+`
		ORDER BY s.path, s.line`, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	byFile := map[string][]tag{}
	for rows.Next() {
		var p, name, kind string
		var line, exported int
		if err := rows.Scan(&p, &name, &kind, &line, &exported); err != nil {
			return "", err
		}
		byFile[p] = append(byFile[p], tag{name: name, kind: kind, line: line, exported: exported == 1})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Contents of %s/:\n", relPath)
	for _, p := range paths {
		fmt.Fprintf(&b, "\n%s\n", p)
		writeTags(&b, byFile[p])
	}
	return capOutput(b.String(), maxTokens), nil
}

// Symbol finds definitions of name plus the files that reference the
// files defining it.
func (s *Service) Symbol(ctx context.Context, name string, maxTokens int) (string, error) {
	if err := s.init(); err != nil {
		return "", err
	}
	s.ensureStarted(ctx)
	defs, err := s.symbolDefs(ctx, name)
	if err != nil {
		return "", err
	}
	if len(defs) == 0 {
		// A miss can mean the symbol was added to an already-indexed
		// file after the last walk — stat-scan for dirty files once
		// before reporting the miss. Brand-new files still require a
		// re-walk; that is the as-of-build boundary.
		s.refreshDirty(ctx)
		defs, err = s.symbolDefs(ctx, name)
		if err != nil {
			return "", err
		}
	}
	if len(defs) == 0 {
		return fmt.Sprintf("No symbol named %q in the index.", name), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Symbol %q — %d definition(s):\n", name, len(defs))
	for _, d := range defs {
		vis := "unexported"
		if d.tag.exported {
			vis = "exported"
		}
		fmt.Fprintf(&b, "  %s:%d  %s (%s)\n", d.path, d.tag.line, d.tag.kind, vis)
	}

	// Referrers: files whose refs point at a defining file or its dir.
	seen := map[string]bool{}
	for _, d := range defs {
		dir := filepath.ToSlash(filepath.Dir(d.path))
		rows, err := s.db.QueryContext(ctx, `
			SELECT DISTINCT src_path FROM refs
			WHERE dst_path = ? OR dst_path = ?`, d.path, dir)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var src string
			if err := rows.Scan(&src); err == nil && !seen[src] {
				seen[src] = true
			}
		}
		rows.Close()
	}
	if len(seen) > 0 {
		var refs []string
		for r := range seen {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		fmt.Fprintf(&b, "\nReferenced by %d file(s):\n", len(refs))
		for _, r := range refs {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}
	return capOutput(b.String(), maxTokens), nil
}

// symbolDefs returns name's definition sites. Candidate paths are
// refreshed before the rows are materialized — refreshing after the
// select would render pre-refresh data.
func (s *Service) symbolDefs(ctx context.Context, name string) ([]symDef, error) {
	pathRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT path FROM symbols WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	var cand []string
	for pathRows.Next() {
		var p string
		if err := pathRows.Scan(&p); err != nil {
			pathRows.Close()
			return nil, err
		}
		cand = append(cand, p)
	}
	pathRows.Close()
	s.refreshPaths(ctx, cand)

	rows, err := s.db.QueryContext(ctx, `
		SELECT path, name, kind, line, exported FROM symbols
		WHERE name = ? ORDER BY exported DESC, path`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var defs []symDef
	for rows.Next() {
		var p, n, k string
		var line, exp int
		if err := rows.Scan(&p, &n, &k, &line, &exp); err != nil {
			return nil, err
		}
		defs = append(defs, symDef{p, tag{name: n, kind: k, line: line, exported: exp == 1}})
	}
	return defs, rows.Err()
}

// renderDirTree writes the two-level directory overview with file
// counts, capped at maxOut bytes.
func (s *Service) renderDirTree(ctx context.Context, b *strings.Builder, maxOut int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			CASE WHEN instr(path, '/') = 0 THEN '.'
			     ELSE substr(path, 1, instr(path, '/') - 1) END AS top,
			CASE WHEN instr(path, '/') = 0 THEN '.'
			     WHEN instr(substr(path, instr(path, '/') + 1), '/') = 0
			     THEN substr(path, 1, instr(path, '/') - 1)
			     ELSE substr(path, 1, instr(path, '/') - 1) ||
			          '/' || substr(substr(path, instr(path, '/') + 1), 1,
			          instr(substr(path, instr(path, '/') + 1), '/') - 1)
			END AS dir,
			COUNT(*)
		FROM files GROUP BY dir ORDER BY dir`)
	if err != nil {
		return err
	}
	defer rows.Close()

	b.WriteString("\nLayout:\n")
	start := b.Len()
	for rows.Next() {
		var top, dir string
		var n int
		if err := rows.Scan(&top, &dir, &n); err != nil {
			return err
		}
		if b.Len()-start > maxOut {
			b.WriteString("  …\n")
			break
		}
		fmt.Fprintf(b, "  %s/ (%d)\n", dir, n)
	}
	return rows.Err()
}

// renderTopFiles writes the most-referenced files with their exported
// symbols.
func (s *Service) renderTopFiles(ctx context.Context, b *strings.Builder, limit int) error {
	// In-degree sums every ref row hitting the file itself or one of
	// its ancestor dirs (Go refs resolve to package dirs) — SUM keeps
	// the score deterministic when a file matches several dst_paths.
	// The substr test is a literal prefix match so metacharacters in
	// stored dst_paths can't act as a GLOB pattern.
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.path, COALESCE(SUM(deg.n), 0) AS in_degree
		FROM files f
		LEFT JOIN (
			SELECT dst_path, COUNT(*) AS n FROM refs GROUP BY dst_path
		) deg ON deg.dst_path = f.path
		     OR substr(f.path, 1, length(deg.dst_path) + 1) = deg.dst_path || '/'
		GROUP BY f.path
		ORDER BY in_degree DESC, f.path
		LIMIT ?`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()

	var paths []string
	deg := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			return err
		}
		paths = append(paths, p)
		deg[p] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	s.refreshPaths(ctx, paths)

	b.WriteString("\nMost-referenced files:\n")
	for _, p := range paths {
		syms, err := s.fileSymbols(ctx, p, true)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "  %s (%d refs)", p, deg[p])
		if len(syms) > 0 {
			names := make([]string, 0, len(syms))
			for _, t := range syms {
				names = append(names, t.name)
			}
			fmt.Fprintf(b, " — %s", strings.Join(names, ", "))
		}
		b.WriteString("\n")
	}
	return nil
}

// fileSymbols returns a file's tags, exported-first.
func (s *Service) fileSymbols(ctx context.Context, path string, exportedOnly bool) ([]tag, error) {
	q := `SELECT name, kind, line, exported FROM symbols WHERE path = ?`
	if exportedOnly {
		q += ` AND exported = 1`
	}
	q += ` ORDER BY line`
	rows, err := s.db.QueryContext(ctx, q, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tag
	for rows.Next() {
		var name, kind string
		var line, exp int
		if err := rows.Scan(&name, &kind, &line, &exp); err != nil {
			return nil, err
		}
		out = append(out, tag{name: name, kind: kind, line: line, exported: exp == 1})
	}
	return out, rows.Err()
}

// refreshPaths lazily re-tags result paths whose mtime changed since
// indexing — bounded to the result set, never a full rescan.
func (s *Service) refreshPaths(ctx context.Context, paths []string) {
	for _, p := range paths {
		info, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(p)))
		if errors.Is(err, fs.ErrNotExist) {
			s.dropFile(ctx, p) // Gone on disk — drop the ghost row.
			continue
		}
		if err != nil {
			continue // Transient stat failure — keep the stale row.
		}
		s.refreshIfStale(ctx, p, info.ModTime().UnixNano(), info.Size())
	}
}

func writeTags(b *strings.Builder, tags []tag) {
	for _, t := range tags {
		marker := " "
		if t.exported {
			marker = "+"
		}
		fmt.Fprintf(b, "  %s %4d  %-7s %s\n", marker, t.line, t.kind, t.name)
	}
}

// escapeLike escapes LIKE metacharacters (and the escape char itself)
// so a caller-supplied path can't act as a pattern.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func maxChars(maxTokens int) int {
	if maxTokens <= 0 {
		maxTokens = 500
	}
	return maxTokens * 4
}

func capOutput(s string, maxTokens int) string {
	max := maxChars(maxTokens)
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n\n[Map truncated to stay within token budget]"
}
