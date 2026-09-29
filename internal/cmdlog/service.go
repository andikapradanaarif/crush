// Package cmdlog provides project-scoped command and failure memory:
// the deterministic write path that records what ran, how it ended,
// and which failures are still open. Project-scoped, not
// session-scoped — the tables outlive the session that wrote them, so
// "run the tests" and "fix the failing test" resolve across sessions.
package cmdlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/redact"
	"github.com/charmbracelet/crush/internal/toolclass"
)

// Bounds on stored text: a headline is a lookup hint, not a log, and
// the normalized command is a join key.
const (
	maxCmdNormRunes  = 500
	maxHeadlineRunes = 200
	maxFilesRecorded = 5
	failureScanLines = 40
	defaultListLimit = 20
)

// interruptedExit is the sentinel passed for runs whose verdict never
// arrived (interrupt) — the ledger notes the run without overwriting
// the command's last real exit code.
const interruptedExit = -1

// Service defines the command/failure memory write and read path.
type Service interface {
	// RecordRun records one completed command run: upserts the
	// command ledger, and on a non-zero exit upserts a failure row —
	// on a clean exit it resolves the command's open failures in the
	// same directory. An interrupted run updates the ledger only: a
	// cancelled command is neither a project failure nor a
	// resolution.
	RecordRun(ctx context.Context, run Run)

	// ListCommands returns the project command ledger, most recent
	// first, capped at limit.
	ListCommands(ctx context.Context, limit int) ([]Command, error)

	// ListOpenFailures returns failures with no resolving run,
	// most recently seen first, capped at limit.
	ListOpenFailures(ctx context.Context, limit int) ([]Failure, error)
}

// Run is one completed command invocation.
type Run struct {
	SessionID string
	Command   string
	// CWD is the directory the command's output paths resolve
	// against — the shell's post-run directory, not the launch dir,
	// so "cd x && go test" keys failures under x.
	CWD      string
	Stdout   string
	Stderr   string
	Err      error
	ExitCode int
	// Ran reports the command actually executed and the shell
	// reported a real exit status. A policy denial, parse error, or
	// context kill is not a project failure — it never ran.
	Ran         bool
	Interrupted bool
}

// Command is one command_memory row: a normalized command in a
// workspace-relative directory, and its running tally.
type Command struct {
	CmdNorm string
	CWD     string
	Kind    string
	// LastExit is the command's last real verdict. The -1 sentinel
	// means the last run never produced one (interrupt or denial) —
	// != 0 alone is not a failure signal.
	LastExit      int64
	LastAt        time.Time
	OKCount       int64
	FailCount     int64
	LastSessionID string
}

// Failure is one failure_memory row: a (command, cwd, headline)
// signature and its resolution state.
type Failure struct {
	Signature  string
	Cmd        string
	CWD        string
	Headline   string
	Files      []string
	FirstSeen  time.Time
	LastSeen   time.Time
	ResolvedIn string
}

type service struct {
	q          *db.Queries
	workingDir string
}

// NewService creates the command/failure memory service rooted at
// workingDir, so failure rows key directories the way filetracker
// keys files — workspace-relative, cwd-independent.
func NewService(q *db.Queries, workingDir string) Service {
	if workingDir == "" {
		slog.Warn("Cmdlog got an empty workspace root; cwd keys will follow the process working directory")
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = filepathext.Canonical(abs)
	}
	return &service{q: q, workingDir: workingDir}
}

func (s *service) RecordRun(ctx context.Context, run Run) {
	// The tool's ctx dies with the run — a cancel racing the
	// completion must not drop the memory write.
	ctx = context.WithoutCancel(ctx)
	command, runDir := foldLeadingChdir(run.Command, run.CWD)
	cmdNorm := normalizeCommand(command)
	if cmdNorm == "" {
		return
	}
	cwd := s.relDir(runDir)
	// A run has a verdict only if it executed to an exit status and
	// was not interrupted. Denied, unparseable, or killed commands
	// never ran — the ledger notes them but they open no failure and
	// resolve none.
	interrupted := run.Interrupted || run.ExitCode == 130 // SIGINT by convention, ctx path or not.
	verdict := run.Ran && !interrupted
	ok, fail := int64(0), int64(0)
	lastExit := int64(run.ExitCode)
	switch {
	case !verdict:
		lastExit = interruptedExit
	case run.ExitCode == 0:
		ok = 1
	default:
		fail = 1
	}
	if err := s.q.UpsertCommandRun(ctx, db.UpsertCommandRunParams{
		CmdNorm:       cmdNorm,
		Cwd:           cwd,
		Kind:          toolclass.CommandKind(command),
		LastExit:      lastExit,
		LastAt:        time.Now().UnixMilli(),
		OkCount:       ok,
		FailCount:     fail,
		LastSessionID: run.SessionID,
	}); err != nil {
		slog.Error("Error recording command run", "error", err)
		return
	}
	if !verdict {
		return
	}
	if run.ExitCode == 0 {
		if err := s.q.ResolveFailuresForCommand(ctx, db.ResolveFailuresForCommandParams{
			ResolvedIn: run.SessionID,
			Cmd:        cmdNorm,
			Cwd:        cwd,
		}); err != nil {
			slog.Error("Error resolving failures", "error", err)
		}
		return
	}
	headline := failureHeadline(run.Stderr, run.Stdout, run.Err)
	now := time.Now().UnixMilli()
	if err := s.q.UpsertFailure(ctx, db.UpsertFailureParams{
		Signature: failureSignature(cmdNorm, cwd, headline),
		Cmd:       cmdNorm,
		Cwd:       cwd,
		Headline:  headline,
		Files:     s.failureFilesJSON(run.Stderr, run.Stdout, cwd),
		FirstSeen: now,
		LastSeen:  now,
	}); err != nil {
		slog.Error("Error recording failure", "error", err)
	}
}

func (s *service) ListCommands(ctx context.Context, limit int) ([]Command, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.q.ListRecentCommands(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("listing commands: %w", err)
	}
	out := make([]Command, 0, len(rows))
	for _, r := range rows {
		out = append(out, Command{
			CmdNorm:       r.CmdNorm,
			CWD:           r.Cwd,
			Kind:          r.Kind,
			LastExit:      r.LastExit,
			LastAt:        time.UnixMilli(r.LastAt),
			OKCount:       r.OkCount,
			FailCount:     r.FailCount,
			LastSessionID: r.LastSessionID,
		})
	}
	return out, nil
}

func (s *service) ListOpenFailures(ctx context.Context, limit int) ([]Failure, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.q.ListOpenFailures(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("listing open failures: %w", err)
	}
	out := make([]Failure, 0, len(rows))
	for _, r := range rows {
		out = append(out, Failure{
			Signature:  r.Signature,
			Cmd:        r.Cmd,
			CWD:        r.Cwd,
			Headline:   r.Headline,
			Files:      parseFilesJSON(r.Files),
			FirstSeen:  time.UnixMilli(r.FirstSeen),
			LastSeen:   time.UnixMilli(r.LastSeen),
			ResolvedIn: r.ResolvedIn,
		})
	}
	return out, nil
}

// relDir normalizes a run's working directory to workspace-relative so
// "npm test" in packages/api and packages/web are different rows —
// the same command in a different directory is a different failure
// scope, not a fuzzy match.
func (s *service) relDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	clean := filepathext.Canonical(cwd)
	rel, err := filepath.Rel(s.workingDir, clean)
	if err != nil {
		return clean
	}
	return rel
}

// foldLeadingChdir turns "cd dir && cmd" and "cd dir; cmd" into
// (cmd, cwd+dir): the directory the command runs in is scope, not
// command identity — "npm test" launched under working_dir=api and
// "cd api && npm test" launched at root are the same run. Only a
// single leading cd segment folds; anything else keeps both.
func foldLeadingChdir(command, cwd string) (string, string) {
	m := leadingChdirPattern.FindStringSubmatch(command)
	if m == nil {
		return command, cwd
	}
	dir := strings.Trim(m[1], `"'`)
	rest := strings.TrimSpace(m[3])
	// Only fold a dir spelling the pattern can resolve: "cd -"
	// (OLDPWD), "cd ~", and "cd $VAR" would key a bogus scope.
	if rest == "" || dir == "-" || dir == "" || dir[0] == '~' || dir[0] == '$' {
		return command, cwd
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return rest, dir
}

// leadingChdirPattern matches "cd DIR &&" or "cd DIR;" at the head of
// a command. DIR is one arg: quoted, or a bare field. A bare cd (no
// arg) changes nothing — $HOME folds back to itself.
var leadingChdirPattern = regexp.MustCompile(`^\s*cd\s+((?:"[^"]+")|(?:'[^']+')|[^\s;&|]+)\s*(&&|;)\s*(.*)$`)

// normalizeCommand is the join key for command memory: whitespace-
// collapsed, redacted (a command can carry a credential inline), and
// length-bounded so cosmetic respellings share a row.
func normalizeCommand(command string) string {
	return truncateRunes(redact.Secrets(strings.Join(strings.Fields(command), " ")), maxCmdNormRunes)
}

// failureHeadline picks the most informative line of a failing run:
// a verdict-shaped line, stderr first then stdout — most test
// runners (go test, pytest, npm test) write FAIL lines to stdout
// while routine warnings crowd stderr, so a content-fallback on
// stderr would shadow stdout's real verdict and collapse every
// distinct failure onto one signature. Only when neither stream
// carries a verdict does arbitrary content headline. The exec error
// is the last resort. Redacted before it touches a durable row.
func failureHeadline(stderr, stdout string, runErr error) string {
	headline := firstVerdictLine(stderr)
	if headline == "" {
		headline = firstVerdictLine(stdout)
	}
	if headline == "" {
		headline = firstContentLine(stderr)
	}
	if headline == "" {
		headline = firstContentLine(stdout)
	}
	if headline == "" && runErr != nil {
		headline = firstContentLine(runErr.Error())
	}
	return truncateRunes(redact.Secrets(headline), maxHeadlineRunes)
}

// failureSignature is the dedupe key for a failure: normalized command
// + directory + headline with positional noise stripped — line
// references, durations, and hex addresses churn between runs of the
// same failure — while digits that name the failure itself
// (TestParse2 vs TestParse3) stay part of the identity. The same
// command failing in a sibling directory is a distinct row.
func failureSignature(cmdNorm, cwd, headline string) string {
	stable := lineRefPattern.ReplaceAllString(headline, "")
	stable = durationPattern.ReplaceAllString(stable, "()")
	stable = hexAddrPattern.ReplaceAllString(stable, "0x")
	sum := sha256.Sum256([]byte(cmdNorm + "\x00" + cwd + "\x00" + stable))
	return hex.EncodeToString(sum[:8])
}

var (
	// Signature noise patterns: a :line[:col] reference, a (0.00s)
	// duration, or a 0x address changes run to run; digits inside
	// identifiers (TestParse2) are identity, not noise.
	lineRefPattern  = regexp.MustCompile(`:\d+(:\d+)?`)
	durationPattern = regexp.MustCompile(`\(\s*\d+(?:\.\d+)?\s*(?:ns|µs|ms|s|m|h)?\s*\)`)
	hexAddrPattern  = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	// failureLinePattern matches the lines that carry a verdict —
	// FAIL marks, error headlines, panics, or a file:line:col:
	// diagnostic (go build errors carry no "error" word) — preferred
	// over arbitrary first lines so an "ok pkg" prelude or a
	// "# pkg" banner doesn't headline.
	failureLinePattern = regexp.MustCompile(`(?i)\b(?:fail(?:ed|ure)?|error|panic|assert)\b|\b[A-Za-z_]+(?:Error|Exception)\b|exit status \d+|[\w./\\-]+\.[a-z0-9]+:\d+(:\d+)?:`)
	// fileTokenPattern matches path-shaped tokens: segments joined by
	// / or \ with a dotted extension, optionally carrying :line or a
	// Windows drive-letter prefix. The extension starts lowercase so
	// dotted identifiers like errors.New or filepath.Base are not
	// files.
	fileTokenPattern = regexp.MustCompile(`(?:[A-Za-z]:[\\/])?[\w.-]+(?:[/\\][\w.-]+)*\.[a-z][a-z0-9]{0,5}(?::\d+){0,2}`)
	// lineSuffix strips a trailing :line[:col] — but not a Windows
	// drive letter, which is a colon before the path, not after it.
	lineSuffix = regexp.MustCompile(`:\d+(:\d+)?$`)
)

// firstVerdictLine scans only for verdict-shaped lines — it does not
// fall back to arbitrary content, so a noisy "npm warn" on stderr can
// never shadow a FAIL line waiting in stdout.
func firstVerdictLine(s string) string {
	n := 0
	for line := range strings.Lines(s) {
		if n++; n > failureScanLines {
			break
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" && failureLinePattern.MatchString(trimmed) {
			return trimmed
		}
	}
	return ""
}

// failureFilesJSON extracts the file paths a failing run named, from
// both streams, normalized against the run's directory to
// workspace-relative keys — the same spelling file_heat carries, so a
// later join matches. Streams are scanned redacted (a credential
// fragment is not a file hint) and separately so a loud stderr can't
// starve stdout's tokens; candidates must exist on disk, which drops
// hostnames, module paths, and stale references alike.
func (s *service) failureFilesJSON(stderr, stdout, cwd string) string {
	files := s.extractFiles(redact.Secrets(stderr), cwd, nil)
	files = s.extractFiles(redact.Secrets(stdout), cwd, files)
	data, err := json.Marshal(files)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// extractFiles scans output's first lines for path-shaped tokens,
// resolving each against the run directory and keeping only files
// that exist inside the workspace.
func (s *service) extractFiles(output, cwd string, files []string) []string {
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		seen[f] = true
	}
	n := 0
	for line := range strings.Lines(output) {
		if n++; n > failureScanLines {
			break
		}
		for _, tok := range fileTokenPattern.FindAllString(line, -1) {
			path := strings.TrimSpace(lineSuffix.ReplaceAllString(tok, ""))
			if path == "" {
				continue
			}
			// Tokens resolve against the run's directory; the stored
			// key is workspace-relative or nothing.
			if !filepath.IsAbs(path) {
				path = filepath.Join(s.workingDir, cwd, path)
			}
			path = filepathext.Canonical(path)
			rel, err := filepath.Rel(s.workingDir, path)
			// A path must stay inside the workspace — "..foo.go" is a
			// legal workspace file, ".." or "../x" is an escape.
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			// Only real files are hints: "example.com" parses as a
			// path but isn't one.
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				continue
			}
			if seen[rel] {
				continue
			}
			seen[rel] = true
			files = append(files, rel)
			if len(files) >= maxFilesRecorded {
				return files
			}
		}
	}
	if files == nil {
		return []string{}
	}
	return files
}

func parseFilesJSON(raw string) []string {
	var files []string
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return []string{}
	}
	return files
}

func firstContentLine(s string) string {
	for line := range strings.Lines(s) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
