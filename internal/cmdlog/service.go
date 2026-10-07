// Package cmdlog provides project-scoped command and failure memory:
// the deterministic write path that records what ran, how it ended,
// and which failures are still open. Project-scoped, not
// session-scoped — the tables outlive the session that wrote them, so
// "run the tests" and "fix the failing test" resolve across sessions.
//
// Every row carries per-observation provenance (#220): the session and
// tool call that observed it, the repo state it was true of, whether
// the action was memory-suggested (the contamination-screen flag), and
// the partition it belongs to. project_key is the stable repo identity
// — canonical git common-dir plus normalized remote URL — and reads
// treat it as an admissibility clause, not a ranking signal: a row
// from another partition is inadmissible before relevance runs.
package cmdlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/params"
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

// The open-failure TTL defaults live in params.DefaultMemory: the
// bound is a learned-parameter candidate (#228), so the value and
// its versioning come from the resolved parameter set, not a local
// constant. It stays generous on purpose — the cost asymmetry
// favors recall: a stale row costs the agent one verification
// re-run, a missing row costs full rediscovery.

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

	// ListResolvedFailures returns failures a later clean run
	// resolved — knowledge of what passed again, not open
	// warnings — most recently seen first, capped at limit.
	ListResolvedFailures(ctx context.Context, limit int) ([]Failure, error)

	// ListSessionOpenFailures returns the failure rows still open
	// whose recorded command the given session last ran and last
	// failed — the reconcile edge's "observed and left open" set.
	// The session key is command_memory's last-writer stamp, so a
	// row another session re-ran more recently drops out of this
	// session's set even while the row stays open.
	ListSessionOpenFailures(ctx context.Context, sessionID string) ([]Failure, error)

	// MarkSuggested records that the session was shown cmdNorm in an
	// injected memory section this turn. A later run of that command
	// writes suggested=1 — the contamination-screen flag: an action
	// the memory recommended must not feed back as independent
	// evidence.
	MarkSuggested(sessionID, cmdNorm string)

	// ProjectKey is the stable partition identity of the project
	// this store belongs to — the canonical git common-dir (linked
	// worktrees fold into the owning repo) plus the normalized
	// remote URL when one exists, or the workspace path outside a
	// repository.
	ProjectKey() string

	// ParamVersion is the resolved learned-params snapshot's
	// identity (#228) stamped on new rows — "pv1-<hash>" of the
	// params.Memory in force; rows written before the substrate
	// landed carry the "pv0" placeholder.
	ParamVersion() string

	// ForgetSession drops a deleted session's suggested-marks so the
	// bookkeeping doesn't outlive it — same convention as the
	// coordinator's per-session map sweeps.
	ForgetSession(sessionID string)
}

// Run is one completed command invocation.
type Run struct {
	SessionID string
	// ToolCallID is the call whose observation carried this verdict
	// into the ledger — the originating call for synchronous runs,
	// the polling/killing call for background completions. Empty
	// where no tool call exists (user `!` commands, shell runs).
	ToolCallID string
	Command    string
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
	// ComponentExits carries the exit status of each call the
	// interpreter dispatched during the run — pipeline and list
	// elements included — so a composite like "go test | head" or
	// "go test; echo $?" can't launder a real failure behind its
	// clean final status.
	ComponentExits []int
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
	// LastToolCallID, RepoState, and Suggested describe the run that
	// produced the last real verdict — a verdictless run (interrupt,
	// denial) never re-stamps them.
	LastToolCallID string
	RepoState      string
	Suggested      bool
	ProjectKey     string
	ParamVersion   string
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
	// SessionID and ToolCallID identify the observation that opened
	// the row; ResolvedCall names the call that closed it, where
	// detectable.
	SessionID    string
	ToolCallID   string
	RepoState    string
	Suggested    bool
	ProjectKey   string
	ParamVersion string
	ResolvedCall string
}

type service struct {
	q          *db.Queries
	workingDir string
	// projectKey is the partition identity every row reads and
	// writes under; paramVersion is the learned-params snapshot in
	// force (#228), empty until the substrate exists.
	projectKey   string
	paramVersion string
	// hasRepo caches "a repo exists at workingDir" from key
	// derivation, so RecordRun's per-run repo_state lookup can skip
	// the git exec entirely outside a repository — non-git dirs are
	// where it would fail every time.
	hasRepo bool
	// commonDir is the project key's repo component — the canonical
	// git common-dir — kept separately so the sibling re-claim can
	// recognize stale keys that differ only in remote prefix (#266).
	// Empty outside a repository.
	commonDir string
	// openFailureTTL is the read-side staleness bound for open
	// failures; 0 disables the filter.
	openFailureTTL time.Duration
	// suggested is the per-session set of commands the model was
	// shown in injected memory — a run of one writes suggested=1.
	// Process-local: the flag means "influence currently rendered in
	// this process," and a mid-session restart loses marks until the
	// next armed render re-marks — a bounded under-inclusive window
	// the screen accepts.
	suggestedMu sync.Mutex
	suggested   map[string]map[string]struct{}
}

// NewService creates the command/failure memory service rooted at
// workingDir, so failure rows key directories the way filetracker
// keys files — workspace-relative, cwd-independent. p is the
// resolved parameter set: its open-failure TTL bounds the read
// side and its Version stamps every new row's param_version.
func NewService(q *db.Queries, workingDir string, p params.Memory) Service {
	if workingDir == "" {
		slog.Warn("Cmdlog got an empty workspace root; cwd keys will follow the process working directory")
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = filepathext.Canonical(abs)
	}
	p = p.OrDefault()
	projectKey, commonDir, hasRepo := computeProjectKey(workingDir)
	s := &service{
		q:              q,
		workingDir:     workingDir,
		openFailureTTL: p.OpenFailureTTL,
		projectKey:     projectKey,
		commonDir:      commonDir,
		hasRepo:        hasRepo,
		paramVersion:   p.Version(),
		suggested:      map[string]map[string]struct{}{},
	}
	// Backfill: pre-provenance rows belong to this store's project,
	// so empty keys claim into the current partition once. Failure
	// rows need a Go-side re-key — their signature hash carries
	// project_key, so a bare column UPDATE would orphan the row
	// under its pre-partition PK and the next occurrence of the same
	// failure would open a duplicate.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.claimLegacyFailures(ctx); err != nil {
		slog.Warn("Failed to claim failure-memory partition", "error", err)
	}
	if err := s.rekeyCommands(ctx, ""); err != nil {
		slog.Warn("Failed to claim command-memory partition", "error", err)
	}
	if err := s.reclaimSiblingPartitions(ctx); err != nil {
		slog.Warn("Failed to re-claim stale partitions", "error", err)
	}
	return s
}

// claimLegacyFailures moves pre-provenance rows into this partition.
// A re-occurrence post-upgrade already exists under the partitioned
// signature — that twin carries fuller provenance, so it survives,
// inheriting only the older first_seen, and the claimed row goes.
func (s *service) claimLegacyFailures(ctx context.Context) error {
	rows, err := s.q.ListUnclaimedFailures(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := s.rekeyFailure(ctx, r.Signature, r.Cmd, r.Cwd, r.Headline, r.FirstSeen); err != nil {
			return err
		}
	}
	return nil
}

// reclaimSiblingPartitions re-keys rows whose partition shares this
// repo's common-dir but carries a stale remote prefix — the remote
// was added, re-pointed, renamed, or removed after the rows were
// written, so they orphaned into a sibling partition (#266). A key
// only matches when its common-dir component is byte-identical —
// two repos can't share a .git path, so a foreign project's key can
// never sibling-match. This also repairs mid-process remote changes
// on next open: rows kept writing under the cached key until
// restart, and the re-claim gathers them back.
func (s *service) reclaimSiblingPartitions(ctx context.Context) error {
	if s.commonDir == "" {
		return nil
	}
	fKeys, err := s.q.ListFailurePartitionKeys(ctx)
	if err != nil {
		return err
	}
	cKeys, err := s.q.ListCommandPartitionKeys(ctx)
	if err != nil {
		return err
	}
	stale := map[string]struct{}{}
	for _, k := range append(fKeys, cKeys...) {
		if k != s.projectKey && s.siblingKey(k) {
			stale[k] = struct{}{}
		}
	}
	for k := range stale {
		rows, err := s.q.ListFailuresByKey(ctx, k)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := s.rekeyFailure(ctx, r.Signature, r.Cmd, r.Cwd, r.Headline, r.FirstSeen); err != nil {
				return err
			}
		}
		if err := s.rekeyCommands(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// siblingKey reports whether pk names this repo under a different
// remote prefix — bare common-dir (remoteless era) or any
// remote|<same common-dir> form.
func (s *service) siblingKey(pk string) bool {
	return pk == s.commonDir || strings.HasSuffix(pk, "|"+s.commonDir)
}

// rekeyFailure moves one row onto its partitioned signature. An
// already-partitioned twin wins — it carries fresher provenance —
// inheriting only the older first_seen before the stale row goes.
func (s *service) rekeyFailure(ctx context.Context, oldSig, cmd, cwd, headline string, firstSeen int64) error {
	newSig := s.failureSignature(cmd, cwd, headline)
	meta, err := s.q.GetFailureMeta(ctx, newSig)
	switch {
	case err == nil:
		if err := s.q.MergeFailureFirstSeen(ctx, db.MergeFailureFirstSeenParams{
			FirstSeen: min(meta.FirstSeen, firstSeen),
			Signature: newSig,
		}); err != nil {
			return err
		}
		return s.q.DeleteFailure(ctx, oldSig)
	case errors.Is(err, sql.ErrNoRows):
		return s.q.RekeyFailurePartition(ctx, db.RekeyFailurePartitionParams{
			Signature:   newSig,
			ProjectKey:  s.projectKey,
			Signature_2: oldSig,
		})
	default:
		return err
	}
}

// rekeyCommands moves command_memory rows off sourceKey onto the
// current partition. project_key is PK material there, so a row
// colliding with an existing twin goes first — the twin keeps its
// fresher provenance.
func (s *service) rekeyCommands(ctx context.Context, sourceKey string) error {
	if err := s.q.DeleteCommandConflicts(ctx, db.DeleteCommandConflictsParams{
		ProjectKey:   sourceKey,
		ProjectKey_2: s.projectKey,
	}); err != nil {
		return err
	}
	return s.q.RekeyCommandPartition(ctx, db.RekeyCommandPartitionParams{
		ProjectKey:   s.projectKey,
		ProjectKey_2: sourceKey,
	})
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
	// A composite that reported 0 still failed when an inner call
	// did — "| head" truncating output, "; echo $?" swallowing the
	// status. The component log recovers the real verdict; a bare
	// command's components can't disagree with its final code.
	realExit := run.ExitCode
	unknownVerdict := false
	if realExit == 0 {
		realExit, unknownVerdict = launderedComponentExit(command, run.ComponentExits)
	}
	ok, fail := int64(0), int64(0)
	lastExit := int64(realExit)
	switch {
	case !verdict || unknownVerdict:
		// No usable verdict — an inner call died to SIGPIPE/SIGINT
		// behind the clean final code, so the run is noted like an
		// interrupt: the last real verdict stands.
		lastExit = interruptedExit
	case realExit == 0:
		ok = 1
	default:
		fail = 1
	}
	suggested := int64(0)
	if s.wasSuggested(ctx, run.SessionID, cmdNorm) {
		suggested = 1
	}
	repoState := s.headSHA(ctx)
	if err := s.q.UpsertCommandRun(ctx, db.UpsertCommandRunParams{
		CmdNorm:        cmdNorm,
		Cwd:            cwd,
		Kind:           toolclass.CommandKind(command),
		LastExit:       lastExit,
		LastAt:         time.Now().UnixMilli(),
		OkCount:        ok,
		FailCount:      fail,
		LastSessionID:  run.SessionID,
		LastToolCallID: run.ToolCallID,
		RepoState:      repoState,
		Suggested:      suggested,
		ProjectKey:     s.projectKey,
		ParamVersion:   s.paramVersion,
	}); err != nil {
		slog.Error("Error recording command run", "error", err)
		return
	}
	if !verdict || unknownVerdict {
		return
	}
	if realExit == 0 {
		if err := s.q.ResolveFailuresForCommand(ctx, db.ResolveFailuresForCommandParams{
			ResolvedIn:   run.SessionID,
			ResolvedCall: run.ToolCallID,
			Cmd:          cmdNorm,
			Cwd:          cwd,
			ProjectKey:   s.projectKey,
		}); err != nil {
			slog.Error("Error resolving failures", "error", err)
		}
		return
	}
	headline := failureHeadline(run.Stderr, run.Stdout, run.Err)
	now := time.Now().UnixMilli()
	if err := s.q.UpsertFailure(ctx, db.UpsertFailureParams{
		Signature:    s.failureSignature(cmdNorm, cwd, headline),
		Cmd:          cmdNorm,
		Cwd:          cwd,
		Headline:     headline,
		Files:        s.failureFilesJSON(run.Stderr, run.Stdout, cwd),
		FirstSeen:    now,
		LastSeen:     now,
		SessionID:    run.SessionID,
		ToolCallID:   run.ToolCallID,
		RepoState:    repoState,
		Suggested:    suggested,
		ProjectKey:   s.projectKey,
		ParamVersion: s.paramVersion,
	}); err != nil {
		slog.Error("Error recording failure", "error", err)
	}
}

func (s *service) ListCommands(ctx context.Context, limit int) ([]Command, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.q.ListRecentCommands(ctx, db.ListRecentCommandsParams{
		ProjectKey: s.projectKey,
		RowLimit:   int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing commands: %w", err)
	}
	out := make([]Command, 0, len(rows))
	for _, r := range rows {
		out = append(out, Command{
			CmdNorm:        r.CmdNorm,
			CWD:            r.Cwd,
			Kind:           r.Kind,
			LastExit:       r.LastExit,
			LastAt:         time.UnixMilli(r.LastAt),
			OKCount:        r.OkCount,
			FailCount:      r.FailCount,
			LastSessionID:  r.LastSessionID,
			LastToolCallID: r.LastToolCallID,
			RepoState:      r.RepoState,
			Suggested:      r.Suggested != 0,
			ProjectKey:     r.ProjectKey,
			ParamVersion:   r.ParamVersion,
		})
	}
	return out, nil
}

func (s *service) ListOpenFailures(ctx context.Context, limit int) ([]Failure, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.q.ListOpenFailures(ctx, db.ListOpenFailuresParams{
		ProjectKey: s.projectKey,
		RowLimit:   int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing open failures: %w", err)
	}
	return s.failuresFromRows(rows), nil
}

// ListResolvedFailures reads resolved rows through the same staleness
// bound as open ones: last_seen stops moving once a row resolves, so
// resolved knowledge ages out on the same schedule it would have had
// it stayed open — a fix remembered past that window is as suspect as
// the failure itself would be.
func (s *service) ListResolvedFailures(ctx context.Context, limit int) ([]Failure, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := s.q.ListResolvedFailures(ctx, db.ListResolvedFailuresParams{
		ProjectKey: s.projectKey,
		RowLimit:   int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing resolved failures: %w", err)
	}
	return s.failuresFromRows(rows), nil
}

func (s *service) ListSessionOpenFailures(ctx context.Context, sessionID string) ([]Failure, error) {
	rows, err := s.q.ListSessionOpenFailures(ctx, db.ListSessionOpenFailuresParams{
		SessionID:  sessionID,
		ProjectKey: s.projectKey,
	})
	if err != nil {
		return nil, fmt.Errorf("listing session open failures: %w", err)
	}
	return s.failuresFromRows(rows), nil
}

// failuresFromRows maps failure_memory rows to Failures under the
// same staleness bound both read paths share — a row past the TTL is
// a suffix in the freshest-first ordering, so the scan can stop.
func (s *service) failuresFromRows(rows []db.FailureMemory) []Failure {
	out := make([]Failure, 0, len(rows))
	cutoff := time.Now().Add(-s.openFailureTTL).UnixMilli()
	for _, r := range rows {
		if s.openFailureTTL != 0 && r.LastSeen < cutoff {
			break
		}
		out = append(out, Failure{
			Signature:    r.Signature,
			Cmd:          r.Cmd,
			CWD:          r.Cwd,
			Headline:     r.Headline,
			Files:        parseFilesJSON(r.Files),
			FirstSeen:    time.UnixMilli(r.FirstSeen),
			LastSeen:     time.UnixMilli(r.LastSeen),
			ResolvedIn:   r.ResolvedIn,
			SessionID:    r.SessionID,
			ToolCallID:   r.ToolCallID,
			RepoState:    r.RepoState,
			Suggested:    r.Suggested != 0,
			ProjectKey:   r.ProjectKey,
			ParamVersion: r.ParamVersion,
			ResolvedCall: r.ResolvedCall,
		})
	}
	return out
}

// MarkSuggested records a command the model was shown in injected
// memory this session — see the Service contract. The set is
// deliberately over-inclusive (once seen, any later run of that
// command could be memory-informed) because the flag feeds a
// contamination screen, not a ranking signal.
func (s *service) MarkSuggested(sessionID, cmdNorm string) {
	if sessionID == "" || cmdNorm == "" {
		return
	}
	s.suggestedMu.Lock()
	defer s.suggestedMu.Unlock()
	set, ok := s.suggested[sessionID]
	if !ok {
		set = map[string]struct{}{}
		s.suggested[sessionID] = set
	}
	set[cmdNorm] = struct{}{}
}

// wasSuggested reports whether this session — or its parent, for
// delegated runs — was previously shown cmdNorm in an injected
// memory section. A child session's command can still be
// memory-influenced because the parent saw the rows before
// delegating; the flag is a contamination screen, so it errs
// inclusive. One parent level only, matching the task tool's
// nesting depth — only top-level agents render memory sections, so
// a deeper chain cannot exist today.
func (s *service) wasSuggested(ctx context.Context, sessionID, cmdNorm string) bool {
	if sessionID == "" {
		return false
	}
	if s.suggestedFor(sessionID, cmdNorm) {
		return true
	}
	sess, err := s.q.GetSessionByID(ctx, sessionID)
	if err != nil || !sess.ParentSessionID.Valid {
		return false
	}
	return s.suggestedFor(sess.ParentSessionID.String, cmdNorm)
}

func (s *service) suggestedFor(sessionID, cmdNorm string) bool {
	s.suggestedMu.Lock()
	defer s.suggestedMu.Unlock()
	_, ok := s.suggested[sessionID][cmdNorm]
	return ok
}

func (s *service) ForgetSession(sessionID string) {
	s.suggestedMu.Lock()
	defer s.suggestedMu.Unlock()
	delete(s.suggested, sessionID)
}

func (s *service) ProjectKey() string {
	return s.projectKey
}

func (s *service) ParamVersion() string {
	return s.paramVersion
}

// computeProjectKey derives the partition identity for this store:
// the canonical git common-dir — which folds linked worktrees into
// the owning repository so worktrees share one partition — plus the
// normalized remote URL when one exists. Outside a repository the
// workspace path stands in, so rows still partition per project
// rather than leaking across a shared data directory.
//
// The remote is identity material, not decoration: adding or
// re-pointing it changes the key and orphans prior rows into a
// sibling partition — the common-dir re-claim in NewService gathers
// them back when only the remote prefix changed (#266).
//
// Also returns the common-dir component — the repo's anchor identity,
// empty outside a repository — and whether a repo exists, so
// RecordRun can skip the per-run rev-parse where it would fail.
func computeProjectKey(workingDir string) (key, commonDir string, hasRepo bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	common, err := gitOut(ctx, workingDir, "rev-parse", "--git-common-dir")
	if err != nil || common == "" {
		return filepathext.Canonical(workingDir), "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workingDir, common)
	}
	key = filepathext.Canonical(common)
	if remote, err := gitOut(ctx, workingDir, "remote", "get-url", "origin"); err == nil && remote != "" {
		key = normalizeRemote(remote) + "|" + key
	}
	return key, filepathext.Canonical(common), true
}

// normalizeRemote reduces a git remote URL to host/path form so
// protocol and cosmetic spellings — https vs ssh, a trailing .git —
// name the same repository.
func normalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	// SCP-style ssh: git@host:org/repo.
	if i := strings.Index(remote, "@"); i >= 0 && !strings.Contains(remote, "://") {
		remote = remote[i+1:]
		remote = strings.Replace(remote, ":", "/", 1)
	} else if i := strings.Index(remote, "://"); i >= 0 {
		remote = remote[i+3:]
		// Drop any userinfo or port — the partition cares about the
		// repository, not the credentials that reach it.
		if j := strings.Index(remote, "/"); j >= 0 {
			host := remote[:j]
			if k := strings.Index(host, "@"); k >= 0 {
				host = host[k+1:]
			}
			if k := strings.Index(host, ":"); k >= 0 {
				host = host[:k]
			}
			remote = host + remote[j:]
		}
	}
	remote = strings.TrimSuffix(strings.ToLower(remote), ".git")
	return strings.Trim(remote, "/")
}

// gitOut runs one read-only git query, trimmed and best-effort —
// empty on any failure (not a repo, git absent, timeout). cmd.Dir,
// not -C: argv stays all literals so nothing derived from
// configuration reaches command construction.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// headSHA is the repo state anchor for a recorded run: HEAD at
// observation time, best-effort — empty outside a repository or
// when git is absent. Bounded: RecordRun's ctx is detached from
// cancellation, so the lookup carries its own deadline rather than
// inherit none.
func (s *service) headSHA(ctx context.Context) string {
	if !s.hasRepo {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := gitOut(ctx, s.workingDir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return out
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

// Signal-kill component statuses: a composite that ends clean but
// had a call die to SIGPIPE (its output truncated by a consumer) or
// SIGINT carried no verdict at all — the run is noted but neither
// resolves nor opens failures.
const (
	sigintExit  = 128 + 2
	sigpipeExit = 128 + 13
)

// launderedComponentExit recovers what a composite's clean final
// status hid. Returns (maskedExit, unknownVerdict):
//
//   - (code, false) — a real inner failure the final 0 laundered:
//     "go test | head", "go test; echo $?", a multiline command, or
//     "cmd & wait" where the dispatched element failed.
//   - (0, true) — every nonzero component was a signal kill
//     (SIGPIPE truncation, SIGINT inside): the verdict never
//     arrived, so the caller must neither resolve nor fail.
//   - (0, false) — nothing hid: bare commands never suspect (their
//     exit is the whole verdict), and clean composites pass through.
//
// Control-flow masking ("a || true") still records — the failure is
// fact even if the caller shrugged, and missed failures are
// invisible while extra ones are visible noise.
//
// Known residual: a recovered row keys on the composite spelling —
// "go test | head -5" and bare "go test" are different commands, so
// a clean bare re-run can't resolve the composite's row. Keying on
// the failing segment needs argv recorded per component, which the
// log doesn't carry yet.
func launderedComponentExit(command string, exits []int) (int, bool) {
	// "|;&\n" covers pipes, lists, and background elements; "$(" and
	// backticks cover a substitution's failure inside a succeeding
	// call ("echo \"$(go test)\""). A bare "x=$(cmd)" needs no scan —
	// the assignment inherits cmd's status, nothing is laundered.
	if !strings.ContainsAny(command, "|;&\n`") && !strings.Contains(command, "$(") {
		return 0, false
	}
	sawKill := false
	for _, code := range exits {
		switch code {
		case 0:
		case sigintExit, sigpipeExit:
			sawKill = true
		default:
			// The first real failure wins — signal kills are only
			// unknown verdicts when nothing truly failed.
			return code, false
		}
	}
	return 0, sawKill
}

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
// is the last resort. Screened for instruction-shaped spans, then
// redacted, before it touches a durable row — captured output is
// attacker-controlled text that replays into later prompts.
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
	return truncateRunes(redact.Secrets(ScreenHeadline(headline)), maxHeadlineRunes)
}

// failureSignature is the dedupe key for a failure: the partition +
// normalized command + directory + headline with positional noise
// stripped — line references, durations, and hex addresses churn
// between runs of the same failure — while digits that name the
// failure itself (TestParse2 vs TestParse3) stay part of the
// identity. project_key is part of the hash so a shared store keeps
// two projects' identical failures in separate rows rather than
// upserting over each other's provenance.
func (s *service) failureSignature(cmdNorm, cwd, headline string) string {
	stable := lineRefPattern.ReplaceAllString(headline, "")
	stable = durationPattern.ReplaceAllString(stable, "()")
	stable = hexAddrPattern.ReplaceAllString(stable, "0x")
	sum := sha256.Sum256([]byte(s.projectKey + "\x00" + cmdNorm + "\x00" + cwd + "\x00" + stable))
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
