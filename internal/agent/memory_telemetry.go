package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/redact"
)

// memoryTelemetryHoldoutRate is the fraction of opted-in sessions
// assigned to the injection-off arm — the internal control group that
// turns the log from "was memory used" into "did memory help".
const memoryTelemetryHoldoutRate = 0.1

// memoryTelemetryPromptRunes bounds the prompt written per record —
// the log is append-only with no rotation, so per-record size is the
// only bound on growth. Generous enough that referent-touch analysis
// keeps paths and test names.
const memoryTelemetryPromptRunes = 4096

// memoryTelemetry is the opt-in, local-only usage log for memory
// features (issue #206): append-only JSONL inside the project data
// dir, never transmitted. Two record kinds — session_start (once per
// process per session: workdir, HEAD sha, first prompt) and turn
// (per tail computation: prompt, sections rendered, candidate
// counts, per-reason rejections, candidate ages, the holdout arm).
// Records carry raw signals; deriving acceptance — did first actions
// touch the referents, did the user revise — is analysis-side work
// for eval probes over this file plus the session DB.
type memoryTelemetry struct {
	dataDir string
	workDir string
	// projectKey partitions telemetry the way the store partitions
	// rows (#220): the admissibility clause needs the same identity
	// on both sides or a multi-project analysis can't say which
	// partition a record measured. paramVersion is the learned-params
	// snapshot in force (#228), empty until the substrate exists.
	projectKey   string
	paramVersion string

	mu sync.Mutex
	// seen marks sessions whose session_start record was written this
	// process. arms is the per-session holdout assignment (true =
	// injection suppressed). roll is the holdout coin — injectable so
	// tests can force an arm; the default hashes the session ID.
	seen map[string]bool
	arms map[string]bool
	roll func(sessionID string) float64
}

// newMemoryTelemetry returns the logger, or nil when the option is
// off or the data dir is unknown — a nil logger is a valid disabled
// state, so call sites never branch on the flag themselves.
func newMemoryTelemetry(enabled bool, dataDir, workDir, projectKey, paramVersion string) *memoryTelemetry {
	if !enabled || dataDir == "" {
		return nil
	}
	return &memoryTelemetry{
		dataDir:      dataDir,
		workDir:      workDir,
		projectKey:   projectKey,
		paramVersion: paramVersion,
		seen:         map[string]bool{},
		arms:         map[string]bool{},
		roll:         holdoutRoll,
	}
}

// holdoutOff reports the session's control-arm assignment, coining
// once per session on first call. true means this session must not
// see injected memory — the fetch and selection still run so the
// record captures what the suppressed arm would have said.
func (t *memoryTelemetry) holdoutOff(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	// An eval child must never hold out: a trajectory arm enabling
	// telemetry would otherwise lose ~10% of runs to suppression
	// with no marker in the RunRecord. Records still write — only
	// the coin is suppressed. CRUSH_EVAL_TELEMETRY is pinned for
	// every eval child; CRUSH_EVAL_FLAGS only when the manifest
	// declares flags — check both so a flagless arm is covered too.
	if os.Getenv(EvalTelemetryEnvVar) != "" || os.Getenv(EvalFlagsEnvVar) != "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if off, ok := t.arms[sessionID]; ok {
		return off
	}
	off := t.roll(sessionID) < memoryTelemetryHoldoutRate
	t.arms[sessionID] = off
	return off
}

// holdoutRoll is the default coin — a SHA-256 fraction of the session
// ID. The arm is stable across process restarts (a relaunch cannot
// re-flip a held-out session into the treatment arm) while remaining
// effectively uniform over distinct sessions.
func holdoutRoll(sessionID string) float64 {
	sum := sha256.Sum256([]byte(sessionID))
	return float64(binary.BigEndian.Uint64(sum[:8])) / (1 << 64)
}

// recordTurn writes the session_start record on a session's first
// call, then the per-turn record. agent attributes the record — on
// session_start it names the agent whose first turn produced it;
// each turn line carries its own writer, so a multi-agent session's
// records stay attributable.
// armed is effective arming (flag on AND a store to read), so
// "memory off" stays distinguishable from "armed but found nothing"
// — the same conflation tail.audit refuses to make. The arm label
// lives only on turn records: session_start may be written before
// the coin exists (a session's first turn can be unarmed), so a
// session-level label there could mislabel the eventual arm. Logging
// failures are swallowed — telemetry is observability, never a
// reason to disturb a run.
func (t *memoryTelemetry) recordTurn(sessionID, prompt string, sections []string, candidates []cmdlog.Failure, decisions []FailureDecision, agent string, armed, holdout bool, fetchErr error) {
	if sessionID == "" {
		return
	}
	// Same convention as cmdlog: the file is local-only, but a
	// credential pasted into a prompt still must not persist in
	// plaintext. Redact before truncating so a secret can't be
	// split mid-pattern.
	prompt = truncateTailText(redact.Secrets(prompt), memoryTelemetryPromptRunes)
	t.mu.Lock()
	needStart := !t.seen[sessionID]
	t.mu.Unlock()
	// headSHA spawns git — worst case its 3s timeout would serialize
	// every session's writes if run under the lock. Compute it
	// outside; a racing second caller just redoes the same lookup.
	sha := ""
	if needStart {
		sha = headSHA(t.workDir)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.seen[sessionID] && t.append(map[string]any{
		"type":          "session_start",
		"ts":            time.Now().UnixMilli(),
		"session_id":    sessionID,
		"agent":         agent,
		"workdir":       t.workDir,
		"head_sha":      sha,
		"project_key":   t.projectKey,
		"param_version": t.paramVersion,
		"prompt":        prompt,
		"memory_armed":  armed,
	}) {
		t.seen[sessionID] = true
	}
	turn := map[string]any{
		"type":          "turn",
		"ts":            time.Now().UnixMilli(),
		"session_id":    sessionID,
		"agent":         agent,
		"prompt":        prompt,
		"sections":      telemetrySectionNames(sections),
		"memory_armed":  armed,
		"holdout":       holdout,
		"candidates":    len(candidates),
		"admitted":      countAdmitted(decisions),
		"project_key":   t.projectKey,
		"param_version": t.paramVersion,
	}
	if reasons := rejectionReasons(decisions); len(reasons) > 0 {
		turn["rejections"] = reasons
	}
	if ages := candidateAgesDays(candidates); len(ages) > 0 {
		turn["candidate_age_days"] = ages
	}
	if fetchErr != nil {
		turn["fetch_error"] = fetchErr.Error()
	}
	_ = t.append(turn)
}

// forget drops a deleted session's bookkeeping — the deletion
// watcher calls it so seen/arms don't grow unboundedly across a
// process's lifetime, same contract as the other per-session maps.
func (t *memoryTelemetry) forget(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, sessionID)
	delete(t.arms, sessionID)
}

// append encodes one record as a JSON line, opening and closing the
// file per write — a per-turn write rate makes the extra syscall
// trivial, and holding no handle means a TempDir teardown never
// waits on GC to release the log on Windows. Returns whether the
// write succeeded — session_start retries on a later turn after a
// failure rather than being skipped forever.
func (t *memoryTelemetry) append(rec map[string]any) bool {
	if err := os.MkdirAll(t.dataDir, 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(t.dataDir, "memory-telemetry.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec) == nil
}

// telemetrySectionNames reduces rendered tail blobs to their envelope
// names — the record wants "open_failures rendered", not the text
// (which TailAudit already preserves for eval runs).
func telemetrySectionNames(sections []string) []string {
	names := make([]string, 0, len(sections))
	for _, s := range sections {
		names = append(names, tailSectionName(s))
	}
	return names
}

func countAdmitted(decisions []FailureDecision) int {
	n := 0
	for _, d := range decisions {
		if d.Admit {
			n++
		}
	}
	return n
}

// rejectionReasons tallies non-admit decisions by closed-vocab reason
// — the same strings tail.decisions carries, so analysis groups
// identically across eval records and live logs.
func rejectionReasons(decisions []FailureDecision) map[string]int {
	out := map[string]int{}
	for _, d := range decisions {
		if !d.Admit && d.Reason != "" {
			out[d.Reason]++
		}
	}
	return out
}

// candidateAgesDays converts each candidate's last_seen to days-old —
// the TTL-effectiveness signal: memory that gets used at 29 days says
// something different from memory used at 2 hours.
func candidateAgesDays(candidates []cmdlog.Failure) []float64 {
	ages := make([]float64, 0, len(candidates))
	now := time.Now()
	for _, c := range candidates {
		ages = append(ages, now.Sub(c.LastSeen).Hours()/24)
	}
	return ages
}

// headSHA is the session-start worktree anchor — best-effort, empty
// when the workdir is not a repository or git is absent.
func headSHA(workDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// cmd.Dir, not -C: argv stays all literals so nothing derived
	// from configuration reaches command construction.
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
