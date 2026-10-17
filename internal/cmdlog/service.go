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
	"cmp"
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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/charmbracelet/crush/internal/redact"
	"github.com/charmbracelet/crush/internal/shell"
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
	// resolveScanLimit bounds the open rows a component pass scans —
	// generous against any realistic open set; the list is per-project
	// and ordered most-recent.
	resolveScanLimit = 256
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

	// HeadSHA reads the repo's current HEAD — the stamp the bash
	// tool takes pre-exec so the recorded run attributes only its
	// own HEAD movement (#365). Best-effort: empty outside a
	// repository or when git is absent, which leaves the run on the
	// ledger-baseline fallback.
	HeadSHA(ctx context.Context) string

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

	// MarkSuggestedFile records that the session was shown path as a
	// rendered referent target this turn — the file channel of the
	// same contamination screen (#165): an edit landing on a file the
	// tail itself named is echo, not evidence.
	MarkSuggestedFile(sessionID, path string)

	// WasSuggestedFile reports whether this session — or its parent —
	// was shown path in a rendered referent section. Errs inclusive.
	WasSuggestedFile(ctx context.Context, sessionID, path string) bool

	// RecordReferentEpisode stores one judged vague-prompt→edit
	// observation and runs the promotion check: a mapping whose clean
	// accepted count reaches promoteMin distinct sessions earns its
	// referent_memory row (#165).
	RecordReferentEpisode(ctx context.Context, ep ReferentEpisode, promoteMin int) error

	// RecordReferentEpisodeLabeled is RecordReferentEpisode with the
	// artifact labels written in the same insert — required when
	// ep.CreatedAt anchors at the judged prompt rather than mint
	// time, since a prompt-anchored row can be born due.
	RecordReferentEpisodeLabeled(ctx context.Context, ep ReferentEpisode, in ReferentEpisodeLabelInputs, promoteMin int) error

	// LabelReferentEpisode stamps the artifact signals computable at
	// episode-record time (#294): the added-lines survival
	// fingerprint, the judged turn's cost, the session-end test
	// verdict, and the observation window's edge (#340). Idempotent
	// on the episode unique key.
	LabelReferentEpisode(ctx context.Context, ep ReferentEpisode, in ReferentEpisodeLabelInputs) error

	// SettleReferentLabels freezes the labels of episodes whose
	// observation window closed (#340) — a newer session started or
	// created_at + 24h elapsed — computing hunk survival, external-
	// commit evidence, and the test verdict exactly once. Bounded
	// per call so each turn's pass drains the backlog.
	SettleReferentLabels(ctx context.Context, limit int) error

	// ListReferentEpisodeSourceIDs returns every source message ID a
	// session's episodes were minted under — the referent backfill's
	// dedup set (#368), one read instead of a per-boundary check.
	ListReferentEpisodeSourceIDs(ctx context.Context, sessionID string) ([]string, error)

	// RecordTurnOutcome opens one (session, turn) outcome row —
	// the per-user-turn acceptance unit procedural memory mines
	// (#340). Idempotent on (session_id, turn_id).
	RecordTurnOutcome(ctx context.Context, o TurnOutcome) error

	// HasTurnOutcome reports whether a (session, turn) outcome row
	// exists — the backfill's skip check.
	HasTurnOutcome(ctx context.Context, sessionID, turnID string) (bool, error)

	// ListRecentSessionIDs lists sessions provably in this project —
	// the ledger-membership proof — newest first, excluding the
	// live one. Sessions carry no project column; a session with no
	// project-keyed ledger row is out of the backfill scan set.
	ListRecentSessionIDs(ctx context.Context, excludeID string, limit int) ([]db.ListRecentSessionsRow, error)

	// SettleTurnOutcomes freezes the verdicts of turn rows whose
	// observation window closed — same dual trigger and frozen-once
	// contract as episode settlement.
	SettleTurnOutcomes(ctx context.Context, limit int) error

	// ListReferentCandidates returns promoted phrase→target mappings
	// for the turn's extracted phrases — the referent injection pool,
	// strongest evidence first.
	ListReferentCandidates(ctx context.Context, phrases []string, limit int) ([]Referent, error)

	// UpdateProjectRates folds newly settled referent episodes into
	// the project's decayed Beta-Binomial mass (#296): each new
	// evidence-bearing session discounts stored mass by gamma before
	// its outcomes count. rateMu serializes the fold in-process
	// (#356) — a racing sibling skips its pass rather than
	// double-counting the batch; two crush instances sharing one
	// store can still interleave (bounded, documented residual).
	UpdateProjectRates(ctx context.Context, gamma float64) error

	// ReferentRateLowerBound returns the quantile-q lower bound of
	// the project's referent-acceptance posterior — decayed local
	// mass shrunk onto the pooled global prior. Zero local evidence
	// returns the prior's own bound; an error means the gate can't
	// compute and callers keep incumbent behavior.
	ReferentRateLowerBound(ctx context.Context, q float64) (float64, error)

	// RefreshSessionDigests materializes digests for sessions whose
	// row is missing or stale — the lazy write path run at retrieval
	// time, since sessions never formally end (#164). Bounded by
	// limit per call so a long history amortizes across turns.
	RefreshSessionDigests(ctx context.Context, limit int) error

	// SearchSessionDigests runs the FTS5 path — the prompt's content
	// terms match digest bodies project-scoped, current session
	// excluded. A prompt with no content terms returns nil; the
	// caller falls back to RecentSessionDigests.
	SearchSessionDigests(ctx context.Context, prompt, currentSessionID string, limit int) ([]SessionDigest, error)

	// RecentSessionDigests is the recency fallback for bare
	// continuation prompts — freshest other sessions first.
	RecentSessionDigests(ctx context.Context, currentSessionID string, limit int) ([]SessionDigest, error)

	// DeleteSessionDigest drops a deleted session's digest row and
	// FTS shadow — dead pointers are worse than none.
	DeleteSessionDigest(ctx context.Context, sessionID string) error

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
	// HeadBefore is HEAD stamped by the tool just before the command
	// executed — the primary attribution baseline (#365): the run
	// owns pre..post, so an external commit landing between two
	// recorded runs stays outside every run's range and counts as
	// external acceptance evidence. Empty for recorders that can't
	// stamp (verify gate, server shell, eval seeds) — the ledger's
	// last-recorded head falls back as the catch-up net, unchanged.
	HeadBefore string
	// Components carries each call the interpreter dispatched during
	// the run — argv, dispatch-time directory, and exit status,
	// pipeline and list elements included — so a composite like
	// "go test | head" or "go test; echo $?" can't launder a real
	// failure behind its clean final status, and a passing component
	// can resolve the failure row its own command opened even when
	// the composite spelling differs (#319).
	Components []shell.ComponentCall
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
	// where it would fail every time. Construction-time only: a repo
	// initialized mid-lifetime attributes nothing for this service
	// instance, and a later instance settles those commits as
	// external — parity with the old scheme, whose pre-init runs
	// keyed under the file-path partition anyway.
	hasRepo bool
	// commonDir is the project key's repo component — the canonical
	// git common-dir — kept separately so the sibling re-claim can
	// recognize stale keys that differ only in remote prefix (#266).
	// Empty outside a repository.
	commonDir string
	// repoPrefix is the workspace's path inside its repo ("" at the
	// root) — commit paths emit repo-root-relative while ledger
	// targets are workspace-relative, so evidence reads strip it.
	// Empty outside a repository.
	repoPrefix string
	// openFailureTTL is the read-side staleness bound for open
	// failures; 0 disables the filter.
	openFailureTTL time.Duration
	// referentPromoteHits is the promotion floor the settle-time
	// trigger uses — promotion consumes settled outcomes now, so the
	// check can fire when a clean acceptance freezes, not only when
	// the next accepted episode records.
	referentPromoteHits int
	// suggested is the per-session set of commands the model was
	// shown in injected memory — a run of one writes suggested=1.
	// Process-local: the flag means "influence currently rendered in
	// this process," and a mid-session restart loses marks until the
	// next armed render re-marks — a bounded under-inclusive window
	// the screen accepts.
	suggestedMu sync.Mutex
	suggested   map[string]map[string]struct{}
	// labelMu keeps the detached per-turn maturity pass from
	// overlapping itself — two fast turns can race, and the second
	// pass skips rather than duplicating the batch's work.
	labelMu sync.Mutex
	// rateMu serializes the rate fold's read→write→mark (#356): two
	// detached passes overlapping the upsert→mark window would
	// re-fold the same batch into permanent posterior mass. Same
	// process-local bound as labelMu — two crush instances sharing
	// one store can still interleave, a documented residual the
	// shared-store configuration already accepts elsewhere.
	rateMu sync.Mutex
	// sweepFailMu guards sweepFail: (prev..after) ranges whose
	// attribution sweep already failed once. A second failure of the
	// same range is deterministic, not transient — the baseline
	// stamps past it rather than retrying forever.
	sweepFailMu sync.Mutex
	sweepFail   map[string]struct{}
	// drainMu serializes pending-attribution drains — overlapping
	// drains duplicate rev-list work for idempotent writes, so a
	// busy drain makes the next one skip rather than queue.
	drainMu sync.Mutex
	// drainRangeFn lists one pending range's commits. Nil means the
	// real drainRange — tests inject it to reach the ordinary-failure
	// class, which a live repo can't produce (two resolvable commits
	// never fail rev-list).
	drainRangeFn func(ctx context.Context, prevSha, postSha string) (string, error)
	// now is the timestamp source for every stamp and cutoff the
	// service computes. Production wiring leaves it nil (wall
	// clock); eval seeding injects a controlled clock so scripted
	// history can be backdated — last_seen ages and session spacing
	// become authored inputs rather than wall-time artifacts.
	now func() time.Time
}

// Option customizes the service at construction; production callers
// use none.
type Option func(*service)

// WithClock replaces the service's timestamp source — eval-only
// machinery for scripted seed runs that need authored last_seen
// spacing. A nil clock is ignored.
func WithClock(now func() time.Time) Option {
	return func(s *service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithProjectKey pins the partition identity instead of deriving it
// from the repo remote + common dir. options.project_key wires it —
// the eval harness pins a per-trajectory key so a snapshot-restored
// crush.db reads under the key its rows were written with. An empty
// key is ignored.
func WithProjectKey(key string) Option {
	return func(s *service) {
		if key != "" {
			s.projectKey = key
		}
	}
}

// NewService creates the command/failure memory service rooted at
// workingDir, so failure rows key directories the way filetracker
// keys files — workspace-relative, cwd-independent. p is the
// resolved parameter set: its open-failure TTL bounds the read
// side and its Version stamps every new row's param_version.
func NewService(q *db.Queries, workingDir string, p params.Memory, opts ...Option) Service {
	if workingDir == "" {
		slog.Warn("Cmdlog got an empty workspace root; cwd keys will follow the process working directory")
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = filepathext.Canonical(abs)
	}
	p = p.OrDefault()
	projectKey, commonDir, repoPrefix, hasRepo := computeProjectKey(workingDir)
	s := &service{
		q:                   q,
		workingDir:          workingDir,
		openFailureTTL:      p.OpenFailureTTL,
		referentPromoteHits: p.ReferentPromoteHits,
		projectKey:          projectKey,
		commonDir:           commonDir,
		repoPrefix:          repoPrefix,
		hasRepo:             hasRepo,
		paramVersion:        p.Version(),
		suggested:           map[string]map[string]struct{}{},
		sweepFail:           map[string]struct{}{},
		now:                 time.Now,
	}
	for _, opt := range opts {
		opt(s)
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
//
// crush_commits rides the same sweep: an attributed commit orphaned
// under the stale key leaves the current partition's exclusion set
// empty, and every previously-attributed agent commit reads as
// external acceptance — the false-accept direction, at table scale.
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
	ccKeys, err := s.q.ListCrushCommitPartitionKeys(ctx)
	if err != nil {
		return err
	}
	paKeys, err := s.q.ListPendingAttributionPartitionKeys(ctx)
	if err != nil {
		return err
	}
	stale := map[string]struct{}{}
	for _, k := range slices.Concat(fKeys, cKeys, ccKeys, paKeys) {
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
		if err := s.q.RekeyCrushCommits(ctx, db.RekeyCrushCommitsParams{
			ProjectKey:   s.projectKey,
			ProjectKey_2: k,
		}); err != nil {
			return err
		}
		if err := s.q.RekeyPendingAttribution(ctx, db.RekeyPendingAttributionParams{
			ProjectKey:   s.projectKey,
			ProjectKey_2: k,
		}); err != nil {
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
		realExit, unknownVerdict = launderedComponentExit(command, run.Components)
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
	repoState := s.HeadSHA(ctx)
	// HEAD before this run ran is the primary baseline — the tool
	// stamps it pre-exec, so the sweep's range is the movement this
	// run itself produced (#365). Recorders that can't stamp (verify
	// gate, server shell, eval seeds) carry HeadBefore="" and fall
	// back to the ledger's last-recorded head — the #359 catch-up
	// net, unchanged. ledgerHead also feeds the self-rewrite retry
	// inside recordCrushCommits. Attribution still runs BEFORE the
	// upsert stamps this run's head: a sweep that fails or a crash
	// between stamp and insert would otherwise move the recorded
	// baseline past the unattributed commits permanently, and every
	// one of them would settle as external acceptance. On failure
	// the stamp is suppressed so the baseline stays behind the
	// missed range and the next unstamped run re-sweeps it —
	// over-attribution is the safe direction, stamped-but-
	// unattributed is the false-accept hole.
	ledgerHead := s.lastRecordedHead(ctx)
	prevHead := cmp.Or(run.HeadBefore, ledgerHead)
	ledgerState := repoState
	if !s.recordCrushCommits(ctx, run.SessionID, prevHead, ledgerHead, repoState) {
		// repo_state's only baseline reader is lastRecordedHead; the
		// failure upsert below still carries the true head as
		// observation provenance.
		ledgerState = ""
	}
	if err := s.q.UpsertCommandRun(ctx, db.UpsertCommandRunParams{
		CmdNorm:        cmdNorm,
		Cwd:            cwd,
		Kind:           toolclass.CommandKind(command),
		LastExit:       lastExit,
		LastAt:         s.now().UnixMilli(),
		OkCount:        ok,
		FailCount:      fail,
		LastSessionID:  run.SessionID,
		LastToolCallID: run.ToolCallID,
		RepoState:      ledgerState,
		Suggested:      suggested,
		ProjectKey:     s.projectKey,
		ParamVersion:   s.paramVersion,
	}); err != nil {
		slog.Error("Error recording command run", "error", err)
		return
	}
	// Each clean component is its own pass evidence — a decorated or
	// composite re-run resolves the row its inner command opened even
	// when the composite spelling never matches it. Runs before the
	// composite verdict gate: an interrupted composite can still
	// carry components that provably exited 0.
	s.resolveComponentFailures(ctx, run)
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
	now := s.now().UnixMilli()
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
	cutoff := s.now().Add(-s.openFailureTTL).UnixMilli()
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
// Also returns repoPrefix — the workspace's path inside the repo
// ("" at the root, "sub/dir/" deeper) — because `git log
// --name-only` emits repo-root-relative paths while the ledger's
// targets are workspace-relative; commit evidence is blind without
// the strip.
func computeProjectKey(workingDir string) (key, commonDir, repoPrefix string, hasRepo bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	common, err := gitOut(ctx, workingDir, "rev-parse", "--git-common-dir")
	if err != nil || common == "" {
		return filepathext.Canonical(workingDir), "", "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workingDir, common)
	}
	key = filepathext.Canonical(common)
	if remote, err := gitOut(ctx, workingDir, "remote", "get-url", "origin"); err == nil && remote != "" {
		key = normalizeRemote(remote) + "|" + key
	}
	prefix, _ := gitOut(ctx, workingDir, "rev-parse", "--show-prefix")
	return key, filepathext.Canonical(common), filepath.ToSlash(strings.TrimSpace(prefix)), true
}

// ProjectKeyForDir derives the partition identity a DB opened under
// workingDir would use — the same derivation NewService performs —
// so offline readers (the LTT calibrator, #295) can query a project's
// rows without constructing a service.
func ProjectKeyForDir(workingDir string) string {
	key, _, _, _ := computeProjectKey(workingDir)
	return key
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

// HeadSHA is the repo state anchor: HEAD right now, best-effort —
// empty outside a repository or when git is absent. RecordRun reads
// it post-run for the ledger stamp; the bash tool reads it pre-exec
// so the recorded run can attribute only its own movement (#365).
// Bounded: callers' ctx may be detached from cancellation, so the
// lookup carries its own deadline rather than inherit none.
func (s *service) HeadSHA(ctx context.Context) string {
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

// lastRecordedHead returns HEAD as of this project's most recent
// recorded run — the catch-up baseline recordCrushCommits falls back
// to when a run carries no stamped pre-exec head, and the retry
// range when a stamp's object died inside the run's own rewrite
// (#365). Empty outside a repo or when nothing has recorded yet;
// empty means no attribution for an unstamped run, which errs
// toward treating commits as external — the narrow residual is a
// HEAD-moving run with zero prior recorded runs, e.g. the project's
// very first command.
func (s *service) lastRecordedHead(ctx context.Context) string {
	if !s.hasRepo {
		return ""
	}
	head, err := s.q.LastRecordedRepoHead(ctx, s.projectKey)
	if err != nil {
		return ""
	}
	return head
}

// recordCrushCommits attributes the commits a run produced by HEAD
// movement, not command spelling — rebase, merge, cherry-pick, am,
// commit-tree, sudo-prefixed and chained forms all land the same way
// (#359). Every SHA in prevHead..headAfter is crush-made and excluded
// from external acceptance evidence, so an agent commit can never
// self-certify.
//
// prevHead is the run's pre-exec head when the tool stamped one
// (#365): the range covers exactly the movement this run produced,
// so an external commit landing between two recorded runs stays
// outside every run's range and survives as external acceptance
// evidence — the recall the ledger-baseline sweep lost. It also ends
// the cross-worktree baseline chaining: siblings share a project_key
// but each run sweeps only its own worktree's movement. Recorders
// that can't stamp keep sweeping the ledger baseline (ledgerHead) —
// the catch-up net, unchanged, still conservatively absorbing the
// inter-run gap.
//
// The range spans the run's exec window, so external movement that
// lands while the run executes still attributes to it — a
// long-lived background job's range can cover much of a session.
// That's the same absorption the ledger sweep produced, now bounded
// by job lifetime instead of ledger recency: over-attribution, the
// safe direction. Overlapping stamped runs can likewise
// cross-attribute session_id provenance — harmless, since the
// exclusion consumers read is keyed on project_key, not the
// recording session.
//
// A stamped pre can die inside the run's own range — an amend or
// rebase orphaned the object — so a failed sweep retries once
// against ledgerHead: the widened range still covers the run's
// commits at the cost of over-attributing the gap, the safe
// direction. A second failure lands in attributionFallback. The
// retry is unconditioned on the error class — a transient failure
// retries the wider range on the same dying ctx, which almost
// always fails again and lands in the same fallback one exec
// later; discriminating would need error parsing for no gain.
//
// A stamped run's failed range is unique, so no later stamp
// re-covers it — attributionFallback persists it to
// pending_attribution on the hold arm, and every successful sweep
// drains pending ranges afterward (#372). What still settles as
// external acceptance: a run dying post-exec before RecordRun
// (crash or force-kill in the exec→record gap), and a drained
// range past its retry bound — both bounded misses.
//
// The bool reports whether the sweep completed. false means commits
// in the moved range may have gone unattributed; the caller then
// keeps the recorded baseline behind them so a later unstamped run
// re-sweeps (INSERT OR IGNORE makes a partial or repeated
// attribution idempotent — over-attribution is the safe direction).
// Permanent failure classes don't retry forever — see
// attributionFallback.
func (s *service) recordCrushCommits(ctx context.Context, sessionID, prevHead, ledgerHead, headAfter string) bool {
	if !s.hasRepo {
		return true
	}
	if prevHead == "" || headAfter == "" || prevHead == headAfter {
		s.drainPendingAttribution(ctx, sessionID)
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := gitOut(ctx, s.workingDir, "rev-list", prevHead+".."+headAfter)
	if err != nil && ledgerHead != "" && ledgerHead != prevHead {
		// Usually the stamped pre-run head no longer resolving —
		// the run amended or rebased its own base. Retry once
		// against the ledger baseline: the range widens to include
		// inter-run externals, but the run's commits still
		// attribute and over-attribution is the safe direction.
		prevHead = ledgerHead
		out, err = gitOut(ctx, s.workingDir, "rev-list", prevHead+".."+headAfter)
	}
	if err != nil {
		return s.attributionFallback(ctx, prevHead, headAfter, err)
	}
	if out == "" {
		s.drainPendingAttribution(ctx, sessionID)
		return true
	}
	// The write phase detaches from the caller's ctx with its own
	// budget: a large range can spend most of the 3s in rev-list,
	// and a starved tx would hold the baseline over the same
	// (growing) range — retry once, then sweepFail stamps past the
	// whole range as external. Fresh budget keeps the attribution
	// the sweep already computed from dying in the commit.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer wcancel()
	shas := strings.Split(out, "\n")
	now := s.now().UnixMilli()
	write := func(q *db.Queries) error {
		for _, sha := range shas {
			sha = strings.TrimSpace(sha)
			if sha == "" {
				continue
			}
			if err := q.InsertCrushCommit(wctx, db.InsertCrushCommitParams{
				ProjectKey: s.projectKey,
				Sha:        sha,
				SessionID:  sessionID,
				CreatedAt:  now,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	// The sweep's inserts land as one unit — a timeout mid-range
	// would leave the tail unattributed under an advanced baseline.
	conn := s.q.DB()
	if conn == nil {
		if err := write(s.q); err != nil {
			slog.Error("Error recording crush-made commits", "error", err)
			return false
		}
		s.drainPendingAttribution(ctx, sessionID)
		return true
	}
	tx, err := conn.BeginTx(wctx, nil)
	if err != nil {
		slog.Error("Error opening crush-commit attribution transaction", "error", err)
		return false
	}
	defer tx.Rollback()
	if err := write(s.q.WithTx(tx)); err != nil {
		slog.Error("Error recording crush-made commits", "error", err)
		return false
	}
	if err := tx.Commit(); err != nil {
		slog.Error("Error committing crush-made commits", "error", err)
		return false
	}
	s.drainPendingAttribution(ctx, sessionID)
	return true
}

// attributionFallback decides whether a failed sweep advances the
// recorded baseline anyway. Two failure classes get different
// treatment:
//
//   - Dead baseline: the recorded head no longer resolves — orphaned
//     by rebase+gc, a history rewrite (filter-repo/BFG), or a
//     delete-and-reclone at the same path. Holding it can never
//     recover: every later sweep fails identically while agent
//     commits settle as external acceptance — the false-accept
//     direction this mechanism exists to kill. Stamp the true head
//     and accept the bounded one-time miss; permanent blindness is
//     worse.
//   - Live baseline, sweep still failed: transient (timeout, object
//     store hiccup) or deterministic (a huge divergent pull's range).
//     The baseline holds once so the next run re-sweeps — and the
//     range persists to pending_attribution so a later stamped run's
//     drain re-sweeps what its unique pre would otherwise skip
//     (#372); a range failing a second time is deterministic —
//     holding it forever is the same livelock a dead baseline is —
//     so it stamps too.
//
// The repeat set is process-local: a restart grants one free retry,
// which is the right bound.
func (s *service) attributionFallback(ctx context.Context, prevHead, headAfter string, sweepErr error) bool {
	// rev-parse gets its own budget — the caller's ctx may already be
	// dead (that's one way rev-list fails), and a verify that can't
	// run proves nothing either way.
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := gitOut(vctx, s.workingDir, "rev-parse", "--verify", "--quiet", prevHead+"^{commit}"); err != nil {
		slog.Warn("Commit-attribution baseline no longer resolves — advancing past the dead range once",
			"prev", prevHead, "after", headAfter, "error", sweepErr)
		return true
	}
	key := prevHead + ".." + headAfter
	s.sweepFailMu.Lock()
	_, repeated := s.sweepFail[key]
	s.sweepFail[key] = struct{}{}
	s.sweepFailMu.Unlock()
	if repeated {
		slog.Warn("Commit-attribution sweep failed for this range twice — advancing past it once",
			"prev", prevHead, "after", headAfter, "error", sweepErr)
		return true
	}
	slog.Error("Error sweeping commits for attribution; the recorded baseline stays behind for a re-sweep",
		"error", sweepErr, "prev", prevHead, "after", headAfter)
	s.persistPendingAttribution(prevHead, headAfter)
	return false
}

// pendingAttributionCap bounds the pending_attribution set — inserts
// beyond it evict the oldest rows.
const pendingAttributionCap = 8

// pendingAttributionMaxAttempts mirrors sweepFail's retry-once bound:
// a drained range that fails rev-list twice is deterministic and is
// dropped rather than re-queued forever.
const pendingAttributionMaxAttempts = 2

// drainRangeBudget is each pending range's share of the drain — the
// drain's parent budget scales to len(rows) × this so a few slow
// ranges can't starve the tail into deadline-burned attempts.
const drainRangeBudget = 3 * time.Second

// persistPendingAttribution queues a held range for a later run's
// drain (#372). Best-effort and detached — the caller's ctx may
// already be dead, and a persist failure just keeps today's residual.
func (s *service) persistPendingAttribution(prevSha, postSha string) {
	if prevSha == "" || postSha == "" {
		return
	}
	pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.q.InsertPendingAttribution(pctx, db.InsertPendingAttributionParams{
		ProjectKey: s.projectKey,
		PrevSha:    prevSha,
		PostSha:    postSha,
		CreatedAt:  s.now().UnixMilli(),
	}); err != nil {
		slog.Error("Error queueing failed attribution range for drain", "error", err)
		return
	}
	if err := s.q.EvictPendingAttribution(pctx, db.EvictPendingAttributionParams{
		ProjectKey:   s.projectKey,
		ProjectKey_2: s.projectKey,
		Limit:        pendingAttributionCap,
	}); err != nil {
		slog.Error("Error capping pending attribution ranges", "error", err)
	}
}

// drainPendingAttribution re-sweeps ranges a held baseline persisted
// (#372). It runs only after the run's own sweep succeeded — a failing
// git is the wrong time to pile on more rev-lists — on its own detached
// budget, since the caller's ctx may already be dead. Each pending
// range gets its own bounded rev-list so one dead range can't poison
// the drain: either endpoint failing rev-parse drops the row outright
// (the dead-baseline contract — bounded one-time miss, not permanent
// blindness); a range that fails rev-list burns an attempt and drops
// at the retry bound. Drained SHAs record under the draining run's
// session — the original recording session is gone, and exclusion
// reads are project-scoped. A dropped range can re-persist fresh if a
// later ledger-baseline sweep fails on an overlapping range — the
// fresh row restarts the attempts budget, but re-persist only happens
// on a live hold, so the cycle stays practically bounded.
func (s *service) drainPendingAttribution(ctx context.Context, sessionID string) {
	if !s.hasRepo {
		return
	}
	// One drain at a time — a racing second drain would just redo
	// the same rev-lists; skip rather than queue behind it.
	if !s.drainMu.TryLock() {
		return
	}
	defer s.drainMu.Unlock()
	listRange := s.drainRangeFn
	if listRange == nil {
		listRange = s.drainRange
	}
	// The row list is cheap — fetch it on its own budget, then size
	// the drain's to the rows it returned so every pending range
	// gets a full per-range shot.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), drainRangeBudget)
	rows, err := s.q.ListPendingAttribution(lctx, db.ListPendingAttributionParams{
		ProjectKey: s.projectKey,
		Limit:      pendingAttributionCap,
	})
	lcancel()
	if err != nil {
		slog.Error("Error listing pending attribution ranges", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	// Parent gets one extra range-budget of slack: the loop only
	// starts a row when a full budget remains, so an n-row parent
	// sized at exactly n budgets would starve its last row.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		drainRangeBudget*time.Duration(len(rows)+1))
	defer cancel()
	deadline, hasDeadline := dctx.Deadline()
	now := s.now().UnixMilli()
	for _, r := range rows {
		// Less than one full range budget left — stop rather than
		// burn attempts on rev-lists that die to the parent
		// deadline. Starved rows keep their attempts for the next
		// drain.
		if hasDeadline && time.Until(deadline) < drainRangeBudget {
			break
		}
		del := db.DeletePendingAttributionParams{
			ProjectKey: s.projectKey,
			PrevSha:    r.PrevSha,
			PostSha:    r.PostSha,
		}
		rctx, rcancel := context.WithTimeout(dctx, drainRangeBudget)
		out, rerr := listRange(rctx, r.PrevSha, r.PostSha)
		rcancel()
		switch {
		case errors.Is(rerr, errDeadRange):
			// The base no longer resolves — same contract as the
			// live sweep's dead-baseline arm: drop the row, accept
			// the bounded miss.
			if err := s.q.DeletePendingAttribution(dctx, del); err != nil {
				slog.Error("Error dropping dead pending attribution range", "error", err)
			}
		case rerr != nil && r.Attempts+1 >= pendingAttributionMaxAttempts:
			slog.Warn("Pending attribution range failed past its retry bound — dropping",
				"prev", r.PrevSha, "after", r.PostSha, "error", rerr)
			if err := s.q.DeletePendingAttribution(dctx, del); err != nil {
				slog.Error("Error dropping bounded-out pending range", "error", err)
			}
		case rerr != nil:
			if err := s.q.BumpPendingAttributionAttempts(dctx, db.BumpPendingAttributionAttemptsParams{
				ProjectKey: s.projectKey,
				PrevSha:    r.PrevSha,
				PostSha:    r.PostSha,
			}); err != nil {
				slog.Error("Error bumping pending attribution attempts", "error", err)
			}
		default:
			// Drained — attribute every commit in the range under
			// the draining run's session, then retire the row. A
			// failed insert keeps the row (OR IGNORE makes the retry
			// idempotent) rather than dropping the tail permanently.
			insertFailed := false
			for _, sha := range strings.Split(out, "\n") {
				sha = strings.TrimSpace(sha)
				if sha == "" {
					continue
				}
				if err := s.q.InsertCrushCommit(dctx, db.InsertCrushCommitParams{
					ProjectKey: s.projectKey,
					Sha:        sha,
					SessionID:  sessionID,
					CreatedAt:  now,
				}); err != nil {
					slog.Error("Error inserting drained commit attribution", "error", err)
					insertFailed = true
				}
			}
			if insertFailed {
				continue
			}
			if err := s.q.DeletePendingAttribution(dctx, del); err != nil {
				slog.Error("Error retiring drained attribution range", "error", err)
			}
		}
	}
}

// errDeadRange marks a pending range whose endpoints can no longer
// resolve — the drain drops the row instead of burning an attempt.
var errDeadRange = errors.New("pending attribution range does not resolve")

// drainRange reads one pending range: verify both endpoints resolve,
// then rev-list it. Failures classify as dead-range (drop — either
// dead endpoint makes the range permanently unresolvable) or ordinary
// (count toward the retry bound).
func (s *service) drainRange(ctx context.Context, prevSha, postSha string) (string, error) {
	for _, sha := range []string{prevSha, postSha} {
		if _, err := gitOut(ctx, s.workingDir, "rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
			return "", errDeadRange
		}
	}
	return gitOut(ctx, s.workingDir, "rev-list", prevSha+".."+postSha)
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
// resolveComponentFailures resolves open failure rows on the
// components that exited 0 — the composite's spelling ("go test |
// tail") never matches the row its inner command opened ("go test"),
// and decoration is how models actually re-run things (#319: a row
// no decorated re-run could close trapped sessions in reconcile
// loops). A row matches a component in its own directory by exact
// normalized argv or by flag-insensitive core — argv[0] plus
// non-dash tokens — so "-count=1" and "-v" reruns close the same
// target's row. The core can over-resolve a narrowing rerun
// ("-run=TestX" shares its positional core); a stale open row costs
// an unbounded reconcile loop while an over-resolved row only loses
// a reminder, so the trade favors closing.
func (s *service) resolveComponentFailures(ctx context.Context, run Run) {
	if len(run.Components) == 0 {
		return
	}
	opens, err := s.ListOpenFailures(ctx, resolveScanLimit)
	if err != nil {
		slog.Error("Error listing open failures for component resolve", "error", err)
		return
	}
	if len(opens) == 0 {
		return
	}
	for _, c := range run.Components {
		if c.Exit != 0 || len(c.Args) == 0 {
			continue
		}
		compCmd := normalizeCommand(strings.Join(c.Args, " "))
		if compCmd == "" {
			continue
		}
		dir := s.relDir(c.Dir)
		core := commandCore(compCmd)
		for _, f := range opens {
			if f.CWD != dir {
				continue
			}
			if f.Cmd != compCmd && (core == "" || commandCore(f.Cmd) != core) {
				continue
			}
			if err := s.q.ResolveFailuresForCommand(ctx, db.ResolveFailuresForCommandParams{
				ResolvedIn:   run.SessionID,
				ResolvedCall: run.ToolCallID,
				Cmd:          f.Cmd,
				Cwd:          f.CWD,
				ProjectKey:   s.projectKey,
			}); err != nil {
				slog.Error("Error resolving failure on component pass", "error", err)
			}
		}
	}
}

// commandCore is a command's positional skeleton — argv[0] and the
// non-dash tokens — for matching a re-run that differs only in flag
// decoration against the row it resolves.
func commandCore(cmd string) string {
	fields := strings.Fields(cmd)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if !strings.HasPrefix(f, "-") {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// Control-flow masking ("a || true") still records — the failure is
// fact even if the caller shrugged, and missed failures are
// invisible while extra ones are visible noise.
func launderedComponentExit(command string, comps []shell.ComponentCall) (int, bool) {
	// "|;&\n" covers pipes, lists, and background elements; "$(" and
	// backticks cover a substitution's failure inside a succeeding
	// call ("echo \"$(go test)\""). A bare "x=$(cmd)" needs no scan —
	// the assignment inherits cmd's status, nothing is laundered.
	if !strings.ContainsAny(command, "|;&\n`") && !strings.Contains(command, "$(") {
		return 0, false
	}
	sawKill := false
	for _, c := range comps {
		code := c.Exit
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
