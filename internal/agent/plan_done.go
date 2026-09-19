package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/toolclass"
	"github.com/tidwall/gjson"
)

// planItemState is a plan item's derived effective state at the run
// boundary — the marked status conjoined with its evidence.
type planItemState int

const (
	// planOpen is a non-completed mark — the item still needs work.
	planOpen planItemState = iota
	// planEvidenceBlocked is a completed mark whose bound evidence is
	// pending, failed, or unmet — reported to the model with its reason
	// so the override is never invisible.
	planEvidenceBlocked
)

// planVerdict is one non-done plan item plus the reason and readiness
// the retry prompt renders.
type planVerdict struct {
	item   session.PlanItem
	state  planItemState
	reason string
	// ready reports that every DependsOn target resolved done — the
	// ready-before-blocked ordering the retry prompt applies.
	ready bool
}

// writeEvidence is one write-tool result's recorded verification
// entries, keyed to the path it mutated — the per-path view the
// EvidencePaths rule evaluates.
type writeEvidence struct {
	path   string
	checks []message.VerificationCheck
}

// planEvidence is the session-scoped evidence the done-scan reads off
// stored tool results: the latest instance of each check identity, the
// paths writes landed on, and per-write check entries for covering
// evaluation.
type planEvidence struct {
	latest map[string]message.VerificationCheck
	writes []writeEvidence
	// diag is the session's per-path diagnostics verdicts, keyed by
	// attributed path — a check carrying Path attributes to that file
	// regardless of which write recorded it, so a write that breaks a
	// file it did not touch still blocks that file's bindings; a bare
	// entry attributes to its write's path. diagOrder keeps first-seen
	// order so the reported blocker is stable.
	diag      map[string]message.VerificationCheck
	diagOrder []string
}

// scanPlanEvidence walks stored messages collecting verification
// entries — resolved verdicts land on the originating result via
// writeVerificationOutcomes, so the stored metadata is the final state,
// not the mark-time snapshot. Messages are chronological, so the last
// entry seen per check name is the latest instance.
func scanPlanEvidence(msgs []message.Message, workingDir string) *planEvidence {
	callPaths := map[string][]string{}
	for _, m := range msgs {
		for _, part := range m.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok {
				continue
			}
			switch {
			case tc.Name == tools.RenameToolName || tc.Name == tools.ReplaceSymbolToolName:
				// LSP workspace edits touch arbitrary files — `path`
				// is the search root, not a written file, so there is
				// no reliable path to bind evidence to.
			case tools.WriteToolNames[tc.Name] || tc.Name == tools.DownloadToolName:
				if p := tools.ToolCallFilePath(tc.Input); p != "" {
					callPaths[tc.ID] = append(callPaths[tc.ID], normalizePlanPath(workingDir, p))
				}
			case tc.Name == "bash":
				// Bash redirect writes land on paths the file tools
				// never saw — covering evidence for evidence_paths.
				var params struct {
					Command string `json:"command"`
				}
				if json.Unmarshal([]byte(tc.Input), &params) == nil {
					for _, target := range toolclass.BashRedirectTargets(params.Command) {
						callPaths[tc.ID] = append(callPaths[tc.ID], normalizePlanPath(workingDir, target))
					}
				}
			}
		}
	}
	ev := &planEvidence{
		latest: map[string]message.VerificationCheck{},
		diag:   map[string]message.VerificationCheck{},
	}
	for _, m := range msgs {
		for _, part := range m.Parts {
			tr, ok := part.(message.ToolResult)
			if !ok || tr.IsError {
				continue
			}
			var checks []message.VerificationCheck
			if raw := gjson.Get(tr.Metadata, "verification"); raw.Exists() {
				_ = json.Unmarshal([]byte(raw.Raw), &checks)
			}
			for _, chk := range checks {
				if chk.Check != "" {
					ev.latest[chk.Identity()] = chk
				}
			}
			for _, path := range callPaths[tr.ToolCallID] {
				ev.writes = append(ev.writes, writeEvidence{path: path, checks: checks})
			}
		}
	}
	// Diagnostics supersession is per write path and chronological:
	// the last verdict attributed to a path wins, and a write carrying
	// no diagnostics entry at all clears its own path's verdict —
	// optimistic by design, since latching until a checked write would
	// make bash-heavy fixes unresolvable.
	for _, w := range ev.writes {
		sawDiag := false
		for _, chk := range w.checks {
			if chk.Check != "diagnostics" {
				continue
			}
			p := w.path
			if chk.Path != "" {
				p = normalizePlanPath(workingDir, chk.Path)
			}
			if _, seen := ev.diag[p]; !seen {
				ev.diagOrder = append(ev.diagOrder, p)
			}
			ev.diag[p] = chk
			sawDiag = true
		}
		if !sawDiag {
			delete(ev.diag, w.path)
		}
	}
	return ev
}

// normalizePlanPath renders a declared or written path absolute and
// clean so the two vocabularies compare.
func normalizePlanPath(workingDir, p string) string {
	p = strings.TrimRight(p, "/\\")
	return filepath.Clean(filepathext.SmartJoin(workingDir, p))
}

// pathCovers reports whether a write to w satisfies or is covered by
// the declared path p — exact for file bindings, prefix for directory
// bindings.
func pathCovers(p, w string) bool {
	return w == p || strings.HasPrefix(w, p+string(filepath.Separator))
}

// relPlanPath renders an absolute evidence path relative to the
// working directory for gate feedback — "pkg/f.go" reads better than
// the normalized absolute form.
func relPlanPath(workingDir, abs string) string {
	if rel, err := filepath.Rel(workingDir, abs); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return abs
}

// packageTestCovers reports whether a package-test:<dir> check covers
// the declared path. A file binding is covered by its own directory's
// package test; a directory binding by package tests inside it. File
// vs directory is derived from the observed writes — a path an exact
// write landed on is a file, a path only prefix-covered is a dir —
// so extensionless names like "foo.d" classify correctly.
func packageTestCovers(declared, dir string, declaredIsFile bool) bool {
	if declaredIsFile {
		return filepath.Dir(declared) == dir
	}
	return dir == declared || strings.HasPrefix(dir, declared+string(filepath.Separator))
}

// checkVerdictReason renders a bound check's latest instance as a
// blocking reason, or "" when it resolved green. The three non-green
// states stay distinct: unmet (never ran), pending (not yet resolved),
// unverified (ran, inconclusive), failed.
func checkVerdictReason(name string, latest map[string]message.VerificationCheck) string {
	v, ok := latest[name]
	if !ok {
		return "no check named " + name + " has run (evidence unmet)"
	}
	switch v.State {
	case message.VerificationPassed:
		return ""
	case message.VerificationFailed:
		if v.Detail != "" {
			return name + " failed: " + v.Detail
		}
		return name + " failed"
	case message.VerificationPending:
		return name + " has not resolved"
	default:
		return name + " resolved unverified"
	}
}

// evidenceReason evaluates a completed item's bound evidence: every
// named check's latest instance green, and for each declared path an
// observed write with no covering check failed. Returns "" when the
// evidence is met.
func (e *planEvidence) evidenceReason(item session.PlanItem, workingDir string) string {
	for _, name := range item.EvidenceChecks {
		if r := checkVerdictReason(name, e.latest); r != "" {
			return r
		}
	}
	for _, declared := range item.EvidencePaths {
		p := normalizePlanPath(workingDir, declared)
		landed := false
		declaredIsFile := false
		// Latest covering instance per check name — a stale failed
		// entry on an earlier write is superseded by a later covered
		// write's verdict, the same latest-instance semantics the
		// named-check loop applies.
		latestOnPath := map[string]message.VerificationCheck{}
		var pathCheckOrder []string
		for _, w := range e.writes {
			if !pathCovers(p, w.path) {
				continue
			}
			landed = true
			declaredIsFile = declaredIsFile || w.path == p
			for _, chk := range w.checks {
				if chk.Check == "" || chk.Check == "diagnostics" {
					continue
				}
				if _, seen := latestOnPath[chk.Identity()]; !seen {
					pathCheckOrder = append(pathCheckOrder, chk.Identity())
				}
				latestOnPath[chk.Identity()] = chk
			}
		}
		if !landed {
			return "no write observed on " + declared
		}
		for _, id := range pathCheckOrder {
			v := latestOnPath[id]
			// Name-scoped checks (verify:*, package-test:*) evaluate
			// their session-latest instance — a later write elsewhere
			// may have re-resolved the name.
			if lv, exists := e.latest[id]; exists {
				v = lv
			}
			switch v.State {
			case message.VerificationFailed:
				if v.Detail != "" {
					return "check " + v.Check + " failed on " + declared + ": " + v.Detail
				}
				return "check " + v.Check + " failed on " + declared
			case message.VerificationPending:
				return "check " + v.Check + " has not resolved on " + declared
				// unverified is a weak pass — resolved without a
				// verdict, so it does not block. Blocking it would
				// make path evidence unreachable wherever LSP
				// coverage is absent.
			}
		}
		// Diagnostics verdicts attribute per file: an entry covering
		// the declared path blocks it — including failures a write to
		// another file caused (cross-file breakage), which is exactly
		// what the per-file Path field exists to catch.
		for _, wp := range e.diagOrder {
			v, ok := e.diag[wp]
			if !ok || !pathCovers(p, wp) {
				continue
			}
			switch v.State {
			case message.VerificationFailed:
				if v.Detail != "" {
					return "check diagnostics failed on " + relPlanPath(workingDir, wp) + ": " + v.Detail
				}
				return "check diagnostics failed on " + relPlanPath(workingDir, wp)
			case message.VerificationPending:
				return "check diagnostics has not resolved on " + relPlanPath(workingDir, wp)
			}
		}
		// Covering checks beyond the path's own writes: a package test
		// covers every file in its directory, and configured verify
		// commands gate any mutation — their latest failure covers all
		// declared paths. Sorted so the reported blocker is stable.
		var failedNames []string
		for name, v := range e.latest {
			if v.State == message.VerificationFailed {
				failedNames = append(failedNames, name)
			}
		}
		slices.Sort(failedNames)
		for _, name := range failedNames {
			if strings.HasPrefix(name, "package-test:") {
				dir := normalizePlanPath(workingDir, strings.TrimPrefix(name, "package-test:"))
				if packageTestCovers(p, dir, declaredIsFile) {
					return "covering check " + name + " failed"
				}
			}
			if strings.HasPrefix(name, "verify:") {
				return "covering check " + name + " failed"
			}
		}
	}
	return ""
}

// planVerdicts derives each plan item's effective state and returns the
// non-done ones: open items plus completed marks blocked by their
// evidence. A nil return means no gate signal — empty plan, no toolset
// to reconcile it, or a session the caller can't read. A degraded
// evidence read falls back to mark-only evaluation (today's behavior)
// rather than fabricating unmet evidence.
func (a *sessionAgent) planVerdicts(ctx context.Context, sessionID string) []planVerdict {
	if a.sessions == nil || a.tools == nil {
		return nil
	}
	if !a.hasTool(tools.TodosToolName) {
		// A model that cannot write the list cannot reconcile it — the
		// retry would be a guaranteed thrash.
		return nil
	}
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to load session todos for plan evaluation", "error", err, "session_id", sessionID)
		return nil
	}
	if len(sess.Todos) == 0 {
		return nil
	}

	var ev *planEvidence
	workingDir := ""
	if a.configStore != nil {
		workingDir = a.configStore.WorkingDir()
	}
	if a.messages != nil {
		if msgs, err := a.messages.List(ctx, sessionID); err == nil {
			ev = scanPlanEvidence(msgs, workingDir)
		} else {
			slog.Error("Failed to list messages for plan evidence", "error", err, "session_id", sessionID)
		}
	}

	done := map[string]bool{}
	verdicts := make([]planVerdict, 0, len(sess.Todos))
	for _, item := range sess.Todos {
		if item.Status != session.PlanItemCompleted {
			verdicts = append(verdicts, planVerdict{item: item, state: planOpen})
			continue
		}
		// A completed mark with no evidence bound stays done — legacy
		// items predate binding, and requiring evidence is the
		// declaration-time gate's job, not the run-end scan's.
		reason := ""
		if ev != nil && (len(item.EvidenceChecks) > 0 || len(item.EvidencePaths) > 0) {
			reason = ev.evidenceReason(item, workingDir)
		}
		if reason == "" {
			done[item.ID] = true
			continue
		}
		verdicts = append(verdicts, planVerdict{item: item, state: planEvidenceBlocked, reason: reason})
	}
	for i := range verdicts {
		v := &verdicts[i]
		v.ready = true
		for _, dep := range v.item.DependsOn {
			if !done[dep] {
				v.ready = false
				break
			}
		}
	}
	return verdicts
}
