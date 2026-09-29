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
	SessionID   string
	Command     string
	CWD         string
	Stdout      string
	Stderr      string
	Err         error
	ExitCode    int
	Interrupted bool
}

// Command is one command_memory row: a normalized command and its
// running tally.
type Command struct {
	CmdNorm       string
	Kind          string
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
	cmdNorm := normalizeCommand(run.Command)
	if cmdNorm == "" {
		return
	}
	cwd := s.relDir(run.CWD)
	interrupted := run.Interrupted || run.ExitCode == 130 // SIGINT by convention, ctx path or not.
	ok, fail := int64(0), int64(0)
	lastExit := int64(run.ExitCode)
	switch {
	case interrupted:
		// Neither outcome — the command's verdict never arrived.
		lastExit = interruptedExit
	case run.ExitCode == 0:
		ok = 1
	default:
		fail = 1
	}
	if err := s.q.UpsertCommandRun(ctx, db.UpsertCommandRunParams{
		CmdNorm:       cmdNorm,
		Kind:          toolclass.CommandKind(run.Command),
		LastExit:      lastExit,
		LastAt:        time.Now().Unix(),
		OkCount:       ok,
		FailCount:     fail,
		LastSessionID: run.SessionID,
	}); err != nil {
		slog.Error("Error recording command run", "error", err)
		return
	}
	if interrupted {
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
	if err := s.q.UpsertFailure(ctx, db.UpsertFailureParams{
		Signature: failureSignature(cmdNorm, cwd, headline),
		Cmd:       cmdNorm,
		Cwd:       cwd,
		Headline:  headline,
		Files:     s.failureFilesJSON(run.Stderr, run.Stdout, cwd),
		FirstSeen: time.Now().Unix(),
		LastSeen:  time.Now().Unix(),
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
			Kind:          r.Kind,
			LastExit:      r.LastExit,
			LastAt:        time.Unix(r.LastAt, 0),
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
			FirstSeen:  time.Unix(r.FirstSeen, 0),
			LastSeen:   time.Unix(r.LastSeen, 0),
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

// normalizeCommand is the join key for command memory: whitespace-
// collapsed, redacted (a command can carry a credential inline), and
// length-bounded so cosmetic respellings share a row.
func normalizeCommand(command string) string {
	return truncateRunes(redact.Secrets(strings.Join(strings.Fields(command), " ")), maxCmdNormRunes)
}

// failureHeadline picks the most informative line of a failing run.
// stderr first, then stdout — most test runners (go test, pytest,
// npm test) write their FAIL lines to stdout, so a stderr-only scan
// collapses every distinct failure onto "exit status 1". The exec
// error is the last resort. Redacted before it touches a durable row.
func failureHeadline(stderr, stdout string, runErr error) string {
	headline := firstFailureLine(stderr)
	if headline == "" {
		headline = firstFailureLine(stdout)
	}
	if headline == "" && runErr != nil {
		headline = firstContentLine(runErr.Error())
	}
	return truncateRunes(redact.Secrets(headline), maxHeadlineRunes)
}

// failureSignature is the dedupe key for a failure: normalized command
// + directory + digit-stripped headline, so the same failure with a
// fresh line number or address still joins, while the same command
// failing in a sibling directory stays a distinct row.
func failureSignature(cmdNorm, cwd, headline string) string {
	stable := digitsPattern.ReplaceAllString(headline, "")
	sum := sha256.Sum256([]byte(cmdNorm + "\x00" + cwd + "\x00" + stable))
	return hex.EncodeToString(sum[:8])
}

var (
	digitsPattern = regexp.MustCompile(`\d+`)
	// failureLinePattern matches the lines that carry a verdict —
	// FAIL marks, error headlines, panics — preferred over arbitrary
	// first lines so a passing "ok pkg" prelude doesn't headline.
	failureLinePattern = regexp.MustCompile(`(?i)\b(?:fail(?:ed|ure)?|error|panic|assert)\b|exit status \d+`)
	// fileTokenPattern matches path-shaped tokens: segments joined by
	// / or \ with a dotted extension, optionally carrying :line. The
	// extension starts lowercase so dotted identifiers like
	// errors.New or filepath.Base are not files.
	fileTokenPattern = regexp.MustCompile(`[\w.-]+(?:[/\\][\w.-]+)*\.[a-z][a-z0-9]{0,5}(?::\d+){0,2}`)
	// lineSuffix strips a trailing :line[:col] — but not a Windows
	// drive letter, which is a colon before the path, not after it.
	lineSuffix = regexp.MustCompile(`:\d+(:\d+)?$`)
)

// firstFailureLine scans for a verdict-shaped line first, then falls
// back to the first content line — "FAIL: TestFoo" beats "ok pkg".
func firstFailureLine(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > failureScanLines {
		lines = lines[:failureScanLines]
	}
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" && failureLinePattern.MatchString(trimmed) {
			return trimmed
		}
	}
	return firstContentLine(strings.Join(lines, "\n"))
}

// failureFilesJSON extracts the file paths a failing run named, from
// both streams, normalized against the run's directory to
// workspace-relative keys — the same spelling file_heat carries, so a
// later join matches. Conservative: path-shaped tokens only, deduped,
// capped.
func (s *service) failureFilesJSON(stderr, stdout, cwd string) string {
	files := s.extractFiles(stderr+"\n"+stdout, cwd)
	data, err := json.Marshal(files)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func (s *service) extractFiles(output, cwd string) []string {
	seen := map[string]bool{}
	var files []string
	lines := strings.Split(output, "\n")
	if len(lines) > failureScanLines {
		lines = lines[:failureScanLines]
	}
	for _, line := range lines {
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
			if err != nil || strings.HasPrefix(rel, "..") {
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
