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
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
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

// Service defines the command/failure memory write and read path.
type Service interface {
	// RecordRun records one completed command run: upserts the
	// command ledger, and on a non-zero exit upserts a failure row —
	// on a clean exit it resolves the command's open failures. An
	// interrupted run updates the ledger only: a cancelled command
	// is neither a project failure nor a resolution.
	RecordRun(ctx context.Context, sessionID, command, cwd, stderr string, runErr error, exitCode int, interrupted bool)

	// ListCommands returns the project command ledger, most recent
	// first, capped at limit.
	ListCommands(ctx context.Context, limit int) ([]Command, error)

	// ListOpenFailures returns failures with no resolving run,
	// most recently seen first, capped at limit.
	ListOpenFailures(ctx context.Context, limit int) ([]Failure, error)
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

// Failure is one failure_memory row: a (command, headline) signature
// and its resolution state.
type Failure struct {
	Signature  string
	Cmd        string
	Headline   string
	Files      []string
	FirstSeen  time.Time
	LastSeen   time.Time
	ResolvedIn string
}

type service struct {
	q *db.Queries
}

// NewService creates the command/failure memory service.
func NewService(q *db.Queries) Service {
	return &service{q: q}
}

func (s *service) RecordRun(ctx context.Context, sessionID, command, cwd, stderr string, runErr error, exitCode int, interrupted bool) {
	cmdNorm := normalizeCommand(command)
	if cmdNorm == "" {
		return
	}
	ok, fail := int64(0), int64(0)
	switch {
	case interrupted:
		// Neither outcome — the command's verdict never arrived.
	case exitCode == 0:
		ok = 1
	default:
		fail = 1
	}
	if err := s.q.UpsertCommandRun(ctx, db.UpsertCommandRunParams{
		CmdNorm:       cmdNorm,
		Kind:          toolclass.CommandKind(command),
		LastExit:      int64(exitCode),
		LastAt:        time.Now().Unix(),
		OkCount:       ok,
		FailCount:     fail,
		LastSessionID: sessionID,
	}); err != nil {
		slog.Error("Error recording command run", "error", err)
		return
	}
	if interrupted {
		return
	}
	if exitCode == 0 {
		if err := s.q.ResolveFailuresForCommand(ctx, db.ResolveFailuresForCommandParams{
			ResolvedIn: sessionID,
			Cmd:        cmdNorm,
		}); err != nil {
			slog.Error("Error resolving failures", "error", err)
		}
		return
	}
	headline := failureHeadline(stderr, runErr)
	if err := s.q.UpsertFailure(ctx, db.UpsertFailureParams{
		Signature: failureSignature(cmdNorm, headline),
		Cmd:       cmdNorm,
		Headline:  headline,
		Files:     string(failureFilesJSON(stderr)),
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
			Headline:   r.Headline,
			Files:      parseFilesJSON(r.Files),
			FirstSeen:  time.Unix(r.FirstSeen, 0),
			LastSeen:   time.Unix(r.LastSeen, 0),
			ResolvedIn: r.ResolvedIn,
		})
	}
	return out, nil
}

// normalizeCommand is the join key for command memory: whitespace-
// collapsed and length-bounded so cosmetic respellings share a row.
func normalizeCommand(command string) string {
	norm := truncateRunes(strings.Join(strings.Fields(command), " "), maxCmdNormRunes)
	return norm
}

// failureHeadline picks the most informative line of a failing run —
// the first non-empty stderr line, falling back to the exec error. It
// is redacted before it ever touches a durable row.
func failureHeadline(stderr string, runErr error) string {
	headline := firstContentLine(stderr)
	if headline == "" && runErr != nil {
		headline = firstContentLine(runErr.Error())
	}
	return truncateRunes(redact.Secrets(headline), maxHeadlineRunes)
}

// failureSignature is the dedupe key for a failure: the normalized
// command plus a digit-stripped headline so the same failure with a
// fresh line number or address still joins.
func failureSignature(cmdNorm, headline string) string {
	stable := digitsPattern.ReplaceAllString(headline, "")
	sum := sha256.Sum256([]byte(cmdNorm + "\x00" + stable))
	return hex.EncodeToString(sum[:8])
}

var (
	digitsPattern = regexp.MustCompile(`\d+`)
	// fileTokenPattern matches path-shaped tokens: segments joined by
	// / or \ with a dotted extension, optionally carrying :line.
	fileTokenPattern = regexp.MustCompile(`[\w.-]+(?:[/\\][\w.-]+)*\.[A-Za-z]{1,6}(?::\d+){0,2}`)
	// lineSuffix strips a trailing :line[:col] — but not a Windows
	// drive letter, which is a colon before the path, not after it.
	lineSuffix = regexp.MustCompile(`:\d+(:\d+)?$`)
)

// failureFilesJSON extracts the file paths a failing run named, for
// join-against-file_heat queries later. Conservative: path-shaped
// tokens only, deduped, capped.
func failureFilesJSON(stderr string) []byte {
	files := extractFiles(stderr)
	data, err := json.Marshal(files)
	if err != nil {
		return []byte("[]")
	}
	return data
}

func extractFiles(stderr string) []string {
	seen := map[string]bool{}
	var files []string
	lines := strings.Split(stderr, "\n")
	if len(lines) > failureScanLines {
		lines = lines[:failureScanLines]
	}
	for _, line := range lines {
		for _, tok := range fileTokenPattern.FindAllString(line, -1) {
			path := lineSuffix.ReplaceAllString(tok, "")
			path = strings.TrimSpace(path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			files = append(files, path)
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
