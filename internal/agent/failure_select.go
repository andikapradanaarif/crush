package agent

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/toolclass"
)

// FailureDecision is the task-binding selector's verdict on one open
// failure candidate — whether it reached the prompt, and if not, why.
// The decision list is recorded on the turn's TailAudit so an eval can
// distinguish "the selector ran and rejected every candidate" from
// "no candidates existed"; it is the audit contract issue #207 asks
// for. Wire mirror: eval.FailureDecision.
type FailureDecision struct {
	// Signature is the candidate's stable id from cmdlog.
	Signature string `json:"signature"`
	// Cmd echoes the candidate command so the record reads without a
	// signature lookup.
	Cmd string `json:"cmd,omitempty"`
	// Admit reports whether the failure rendered into the tail.
	Admit bool `json:"admit"`
	// Reason is a closed vocabulary — the first disqualifying check
	// that fired, or "admit". Stable strings: evals and dashboards
	// may group on them.
	Reason string `json:"reason"`
}

// Failure-selection reasons — a closed vocabulary so eval predicates
// and log analysis can group on them.
const (
	failAdmit        = "admit"
	failNegatedScope = "negated_scope" // prompt explicitly excluded this target
	failOutOfScope   = "out_of_scope"  // explicit scope exists and the candidate misses it
	failReferentNone = "referent_none" // ambiguous prompt has no failure-shaped referent
	failKindMismatch = "kind_mismatch" // referent kind (test/build/lint) ≠ command kind
	failNarrowScope  = "narrow_scope"  // subpath-bound candidate under an ambiguous prompt
	failPathGone     = "path_gone"     // every implicated path is absent from the workdir
	failStaleSuspect = "stale_suspect" // an implicated path changed after last_seen
)

// referentKindHints maps "the <noun>" referents to the command kinds
// a plausible failure must have. Failure memory can only be the
// referent of a verification-flavored complaint.
var referentKindHints = map[string][]string{
	"test": {"test"}, "tests": {"test"}, "spec": {"test"},
	"build": {"build"}, "compile": {"build"}, "compilation": {"build"},
	"lint": {"lint"}, "vet": {"lint"}, "warning": {"lint"}, "warnings": {"lint"},
	"bug": {"test", "build", "lint"}, "bugfix": {"test", "build", "lint"},
	"crash": {"test", "build", "lint"}, "error": {"test", "build", "lint"},
	"errors": {"test", "build", "lint"}, "failure": {"test", "build", "lint"},
	"fail": {"test", "build", "lint"}, "panic": {"test", "build", "lint"},
	"regression": {"test", "build", "lint"}, "leak": {"test", "build", "lint"},
	"issue": {"test", "build", "lint"}, "problem": {"test", "build", "lint"},
}

// bareReferentKinds applies when the prompt leans on a bare anaphora
// ("fix it", "this is broken") or a failure cue with no noun — an
// ambiguous "fix it" admits test/build/lint rows but never a failed
// ls or deploy; those bind only under explicit scope.
var bareReferentKinds = []string{"test", "build", "lint"}

// failureCueRe matches failure vocabulary without a definite article
// — "tests are red", "build fails" — so an ambiguous prompt that
// mentions failure at all can bind, while an unrelated request
// ("add a README") rejects every candidate.
var failureCueRe = regexp.MustCompile(`(?i)\b(?:fails?|failed|failing|failure|broke|broken|` +
	`crash(?:es|ed|ing)?|panic(?:s|ked|king)?|regress(?:ed|es|ing|ion)?|red|errors?|erroring|flaky)\b`)

// scopeSlashPathRe matches tokens containing a path separator —
// "decoy/foo.go", "./decoy", "src/pkg/". Requires a word char before
// the first slash so URLs and "//" do not match.
var scopeSlashPathRe = regexp.MustCompile(`[\w.-]+/[\w./~-]*`)

// scopeFileRe matches bare filenames with a source-ish extension so
// "fix main.go" scopes like "fix decoy/main.go" does.
var scopeFileRe = regexp.MustCompile(`\b[\w-]+\.(?:go|py|rs|ts|tsx|js|jsx|mjs|c|cc|cpp|cxx|h|hpp|` +
	`java|kt|rb|sh|bash|zsh|fish|md|json|ya?ml|toml|mod|sum|sql|proto|css|scss|html|vue|svelte|mk|cfg|ini)\b`)

// scopeNegationRe marks a clause as exclusionary — a path inside it is
// scope the user denied, not a target.
var scopeNegationRe = regexp.MustCompile(`(?i)\b(?:not|n't|never|avoid|without|skip|exclude[sd]?|except|leave|outside|untouched|unmodified)\b`)

// cmdPathTokenRe matches command arguments that name a repo-relative
// target: "./decoy", ".", "./...", "decoy/", "/abs/path".
var cmdPathTokenRe = regexp.MustCompile(`^(?:\.{1,2}/\S*|\.{3}|\./|\.$|/\S*|[\w.-]+/\S*)$`)

// promptScope splits the prompt's path mentions into positive scope
// (things the user asked to change) and negative scope (things they
// excluded). A path's polarity comes from negation cues in its own
// clause — "do not touch decoy/" makes decoy negative even though the
// sentence names it.
func promptScope(prompt string) (pos, neg []string) {
	for _, c := range splitClauses(prompt) {
		negated := scopeNegationRe.MatchString(c)
		rest := scopeSlashPathRe.ReplaceAllString(c, " ")
		for _, m := range scopeSlashPathRe.FindAllString(c, -1) {
			if p := normScopePath(m); p != "" {
				if negated {
					neg = append(neg, p)
				} else {
					pos = append(pos, p)
				}
			}
		}
		for _, m := range scopeFileRe.FindAllString(rest, -1) {
			if negated {
				neg = append(neg, m)
			} else {
				pos = append(pos, m)
			}
		}
	}
	return dedupeScope(pos), dedupeScope(neg)
}

// splitClauses breaks a prompt at sentence boundaries — negation in
// one clause must not leak into the next ("fix main.go. Do not touch
// decoy/"). A "." only bounds a clause at whitespace or end of prompt
// so file extensions do not split ("main.go" stays whole).
func splitClauses(prompt string) []string {
	var out []string
	start := 0
	for i, r := range prompt {
		boundary := r == '!' || r == '?' || r == ';' || r == '\n' ||
			(r == '.' && (i+1 == len(prompt) || prompt[i+1] == ' ' || prompt[i+1] == '\t'))
		if boundary {
			out = append(out, prompt[start:i])
			start = i + 1
		}
	}
	if start < len(prompt) {
		out = append(out, prompt[start:])
	}
	return out
}

// normScopePath reduces a matched path to a comparable form: "./x" →
// "x", "decoy/" → "decoy", "decoy/." → "decoy". Returns "" for bare
// "." or "./..." — root references are universal scope, not a named
// constraint.
func normScopePath(p string) string {
	p = strings.Trim(p, `"'`+"`")
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." || p == "..." || p == "" {
		return ""
	}
	return p
}

func dedupeScope(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range in {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// pathsOverlap reports whether a candidate's scope directory and a
// prompt scope path could name the same target — same dir, one
// containing the other, or the root containing anything.
func pathsOverlap(dir, p string) bool {
	if dir == "" || dir == "." {
		return true
	}
	p = strings.TrimSuffix(p, "/")
	if d := path.Dir(p); d == dir || strings.HasPrefix(d, dir+"/") {
		return true
	}
	return dir == p || strings.HasPrefix(dir, p+"/")
}

// dirWithinNegated is the directional version for exclusions: a
// candidate is bound to an excluded path when its scope sits inside
// it ("go test ./decoy" inside "do not touch decoy"), NOT when it
// merely covers it — a root-scope command covers every excluded path
// without being bound to it.
func dirWithinNegated(dir, p string) bool {
	p = strings.TrimSuffix(p, "/")
	if dir == p || strings.HasPrefix(dir, p+"/") {
		return true
	}
	// An excluded file binds rows scoped to its own directory —
	// "do not touch decoy/x.go" still rejects the decoy package row.
	if d := path.Dir(p); d != "." && (dir == d || strings.HasPrefix(dir, d+"/")) {
		return true
	}
	return false
}

// cmdTargets extracts the command's repo-relative target args.
// "go test -count=1 ./decoy" → ["decoy"]; "go test ." → ["."]; a
// command with no path args returns nil — its scope is its CWD.
func cmdTargets(cmd string) []string {
	var out []string
	for _, tok := range strings.Fields(cmd) {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		if !cmdPathTokenRe.MatchString(tok) {
			continue
		}
		t := strings.TrimPrefix(tok, "./")
		if t == "..." || t == "" || t == "." {
			out = append(out, ".")
		} else {
			out = append(out, path.Clean(t))
		}
	}
	return out
}

// relCWD normalizes a recorded CWD against the working directory —
// rows store absolute cwds and "top-level" means the workdir itself.
// A cwd outside the workdir stays non-root: it cannot bind to repo
// scope.
func relCWD(cwd, workDir string) string {
	if cwd == "" || cwd == "." {
		return "."
	}
	if workDir != "" {
		if rel, err := filepath.Rel(workDir, cwd); err == nil {
			if rel == "" {
				return "."
			}
			if !strings.HasPrefix(rel, "..") {
				return rel
			}
			return cwd // outside the workdir — never repo-root scope.
		}
	}
	return cwd
}

// failureDirs is the candidate's binding scope: the dirs its command
// targets plus the dirs its implicated files live in (CWD-joined).
// CWD itself joins only when nothing narrower identifies scope — a
// command run from root that names decoy/ is decoy-scoped, not
// root-scoped.
func failureDirs(f cmdlog.Failure, workDir string) []string {
	var dirs []string
	for _, t := range cmdTargets(f.Cmd) {
		dirs = append(dirs, t)
	}
	cwd := relCWD(f.CWD, workDir)
	for _, file := range f.Files {
		dirs = append(dirs, path.Dir(path.Join(cwd, file)))
	}
	if len(dirs) == 0 {
		return []string{cwd}
	}
	return dedupeScope(dirs)
}

// failureTopLevel reports whether the candidate's command runs at
// project scope — a root target (".", "./...") or no path args at all
// with a root CWD. A subpath-bound row under an ambiguous prompt is a
// wrong-referent risk: it anchors the agent to a corner while the
// declared referent usually lives at top scope.
func failureTopLevel(f cmdlog.Failure, workDir string) bool {
	targets := cmdTargets(f.Cmd)
	if len(targets) == 0 {
		return relCWD(f.CWD, workDir) == "."
	}
	return slices.Contains(targets, ".")
}

// referentKinds resolves the ambiguous prompt's "the <noun>" or bare
// anaphora to the command kinds an open failure may claim. Nil means
// the prompt names a target failure memory cannot supply ("the
// config") or doesn't mention failure at all.
func referentKinds(prompt string) []string {
	if !vagueReferentRe.MatchString(prompt) && !failureCueRe.MatchString(prompt) {
		return nil
	}
	if m := theNounRe.FindStringSubmatch(prompt); m != nil {
		return referentKindHints[strings.ToLower(m[1])]
	}
	return bareReferentKinds
}

// failurePaths maps the candidate's implicated files to plausible
// workdir paths. Captured hints are output-relative, which can mean
// repo-relative or package-relative — both joins are tried so a file
// like "decoy_test.go" from "go test ./decoy" still finds
// "decoy/decoy_test.go".
func failurePaths(f cmdlog.Failure, workDir string) []string {
	var out []string
	var bases []string
	if cwd := relCWD(f.CWD, workDir); cwd != "." {
		bases = append(bases, cwd)
	}
	for _, t := range cmdTargets(f.Cmd) {
		if t != "." {
			bases = append(bases, t)
		}
	}
	bases = append(bases, "")
	for _, file := range f.Files {
		if filepath.IsAbs(file) {
			out = append(out, file)
			continue
		}
		for _, b := range bases {
			out = append(out, filepath.Join(workDir, b, file))
		}
	}
	return out
}

// selectOpenFailures is the deterministic task-binding filter between
// ListOpenFailures and tail rendering. Checks run most-explanatory
// first — explicit user scope beats kind binding beats validity — so
// the recorded reason is the strongest single account of why a
// candidate missed the prompt.
//
// Abstention is a valid outcome: an all-rejected set renders no
// <open_failures> section at all, and the decisions record that the
// selector ran and found nothing bound.
func selectOpenFailures(prompt string, failures []cmdlog.Failure, workDir string) ([]cmdlog.Failure, []FailureDecision) {
	if len(failures) == 0 {
		return nil, nil
	}
	pos, neg := promptScope(prompt)
	explicit := len(pos) > 0
	var kinds []string
	if !explicit {
		kinds = referentKinds(prompt)
	}

	var admitted []cmdlog.Failure
	decisions := make([]FailureDecision, 0, len(failures))
	for _, f := range failures {
		d := FailureDecision{Signature: f.Signature, Cmd: f.Cmd}
		reason := failAdmit
		dirs := failureDirs(f, workDir)

		switch {
		case negatedByAny(dirs, neg):
			reason = failNegatedScope
		case explicit && !overlapsAny(dirs, pos):
			reason = failOutOfScope
		case !explicit && kinds == nil:
			reason = failReferentNone
		case !explicit && !slices.Contains(kinds, toolclass.CommandKind(f.Cmd)):
			reason = failKindMismatch
		case !explicit && !failureTopLevel(f, workDir):
			reason = failNarrowScope
		case allPathsGone(f, workDir):
			reason = failPathGone
		case anyPathNewer(f, workDir):
			reason = failStaleSuspect
		}
		d.Admit = reason == failAdmit
		d.Reason = reason
		decisions = append(decisions, d)
		if d.Admit {
			admitted = append(admitted, f)
		}
	}
	return admitted, decisions
}

func overlapsAny(dirs, paths []string) bool {
	for _, d := range dirs {
		for _, p := range paths {
			if pathsOverlap(d, p) {
				return true
			}
		}
	}
	return false
}

func negatedByAny(dirs, paths []string) bool {
	for _, d := range dirs {
		for _, p := range paths {
			if dirWithinNegated(d, p) {
				return true
			}
		}
	}
	return false
}

// allPathsGone rejects a candidate whose every implicated file is
// absent — the repo state the record describes no longer exists.
// Empty Files or an unknown workDir cannot disprove, so they cannot
// reject.
func allPathsGone(f cmdlog.Failure, workDir string) bool {
	if workDir == "" || len(f.Files) == 0 {
		return false
	}
	for _, p := range failurePaths(f, workDir) {
		if _, err := os.Stat(p); err == nil {
			return false
		}
	}
	return true
}

// anyPathNewer rejects a candidate whose implicated files changed
// after the failure was last recorded — the record's content predates
// the current file state and is suspect (the stale-cell pattern: the
// bug was fixed under a different command form, so the row never
// resolved).
func anyPathNewer(f cmdlog.Failure, workDir string) bool {
	if workDir == "" {
		return false
	}
	for _, p := range failurePaths(f, workDir) {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(f.LastSeen) {
			return true
		}
	}
	return false
}
