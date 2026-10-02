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
// "decoy/foo.go", "./decoy", "src/pkg/". Matched candidates are gated
// by isScopePath so idioms like "and/or" and "pass/fail" don't mint
// bogus scope — a bare word/word token is not a path.
var scopeSlashPathRe = regexp.MustCompile(`[\w.-]+/[\w./~-]*`)

// scopeFileRe matches bare filenames with a source-ish extension so
// "fix main.go" scopes like "fix decoy/main.go" does.
var scopeFileRe = regexp.MustCompile(`\b[\w-]+\.(?:go|py|rs|ts|tsx|js|jsx|mjs|c|cc|cpp|cxx|h|hpp|` +
	`java|kt|rb|sh|bash|zsh|fish|md|json|ya?ml|toml|mod|sum|sql|proto|css|scss|html|vue|svelte|mk|cfg|ini)\b`)

// scopeNegationRe marks a clause as exclusionary — a path after one of
// these cues is scope the user denied, not a target. n't carries no
// leading \b: inside "don't" the n sits mid-word after 'o', so a
// boundary there would never match the most common negation form.
var scopeNegationRe = regexp.MustCompile(`(?i)(?:n't\b|\b(?:not|never|avoid|without|skip|exclud(?:e[sd]?|ing)|except|leave|outside|untouched|unmodified)\b)`)

// scopeResetRe marks cue reset points — a negation ends at a
// conjunction that starts a new directive: "don't touch decoy/, but
// fix main.go" negates decoy only.
var scopeResetRe = regexp.MustCompile(`(?i)\b(?:but|however|instead|then|while|whereas|instead|afterwards?)\b`)

// cmdPathTokenRe matches command arguments that name a repo-relative
// target: "./decoy", ".", "./...", "decoy/", "/abs/path".
var cmdPathTokenRe = regexp.MustCompile(`^(?:\.{1,2}/\S*|\.{3}|\./|\.$|/\S*|[\w.-]+/\S*)$`)

// promptScope splits the prompt's path mentions into positive scope
// (things the user asked to change) and negative scope (things they
// excluded). Polarity is positional: a path is negative iff a negation
// cue precedes it in its clause with no reset conjunction between —
// "fix main.go, do not touch decoy/" keeps main.go positive because
// its own span carries no cue.
func promptScope(prompt, workDir string) (pos, neg []string) {
	for _, c := range splitClauses(prompt) {
		rest := scopeSlashPathRe.ReplaceAllString(c, " ")
		for _, loc := range scopeSlashPathRe.FindAllStringIndex(c, -1) {
			if p := normScopePath(c[loc[0]:loc[1]]); p != "" && isScopePath(c[loc[0]:loc[1]], workDir) {
				if cuePrecedes(c, loc[0]) {
					neg = append(neg, p)
				} else {
					pos = append(pos, p)
				}
			}
		}
		for _, loc := range scopeFileRe.FindAllStringIndex(rest, -1) {
			if cuePrecedes(rest, loc[0]) {
				neg = append(neg, rest[loc[0]:loc[1]])
			} else {
				pos = append(pos, rest[loc[0]:loc[1]])
			}
		}
	}
	return dedupeScope(pos), dedupeScope(neg)
}

// cuePrecedes reports whether the text before offset carries an
// exclusion cue more recent than any reset conjunction.
func cuePrecedes(text string, offset int) bool {
	prefix := text[:offset]
	lastNeg, lastReset := 0, 0
	if idx := scopeNegationRe.FindAllStringIndex(prefix, -1); idx != nil {
		lastNeg = idx[len(idx)-1][1]
	}
	if idx := scopeResetRe.FindAllStringIndex(prefix, -1); idx != nil {
		lastReset = idx[len(idx)-1][1]
	}
	return lastNeg > lastReset
}

// isScopePath gates slash-bearing tokens so idioms ("and/or",
// "pass/fail", "read/write") and URL suffixes never count as scope —
// the one fail-open direction this selector must not take. A token is
// a path when it has a path-only shape (./ ../ / prefix, trailing
// slash, 3+ segments, or a source extension) or exists on disk.
func isScopePath(tok, workDir string) bool {
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") || strings.HasPrefix(tok, "/") {
		return true
	}
	if strings.HasSuffix(tok, "/") || strings.HasSuffix(tok, "/.") {
		return true
	}
	// A dotted first segment reads as a host — "github.com/x/y" is a
	// URL, not a repo path.
	if strings.Contains(tok[:strings.IndexByte(tok, '/')], ".") {
		return false
	}
	if strings.Count(tok, "/") >= 2 {
		return true
	}
	last := tok[strings.LastIndexByte(tok, '/')+1:]
	if scopeFileRe.MatchString(last) && !strings.Contains(last, "/") {
		return true
	}
	if workDir != "" {
		if _, err := os.Stat(filepath.Join(workDir, normScopePath(tok))); err == nil {
			return true
		}
	}
	return false
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
// targets plus the dirs its implicated files live in. File hints are
// stored workspace-relative, so they join scope unmodified. CWD joins
// only when nothing narrower identifies scope — a command run from
// root that names decoy/ is decoy-scoped, not root-scoped.
func failureDirs(f cmdlog.Failure, workDir string) []string {
	var dirs []string
	for _, t := range cmdTargets(f.Cmd) {
		dirs = append(dirs, t)
	}
	for _, file := range f.Files {
		dirs = append(dirs, path.Dir(file))
	}
	if len(dirs) == 0 {
		return []string{relCWD(f.CWD, workDir)}
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

// referentKinds resolves the ambiguous prompt's referents to the
// command kinds an open failure may claim, unioning across every
// "the <noun>" in the prompt — an adjective capture ("the failing
// test" → "failing") falls through to the next word, and a
// non-failure noun never disqualifies a later failure noun. Nil
// means no the-noun bound a kind: either the prompt names targets
// failure memory cannot supply ("the config"), or it never mentions
// failure at all.
func referentKinds(prompt string) []string {
	if !vagueReferentRe.MatchString(prompt) && !failureCueRe.MatchString(prompt) {
		return nil
	}
	matches := theNounRe.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		return bareReferentKinds
	}
	var kinds []string
	for _, m := range matches {
		noun := strings.ToLower(prompt[m[2]:m[3]])
		cands := referentKindHints[noun]
		if cands == nil {
			// Adjective/qualifier form: "the failing test" captured
			// "failing" — try the word after it.
			if next := nextWord(prompt[m[1]:]); next != "" {
				cands = referentKindHints[next]
			}
		}
		for _, k := range cands {
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
	}
	return kinds
}

// nextWord returns the first word in s, lowercased.
func nextWord(s string) string {
	for _, f := range strings.Fields(s) {
		return strings.ToLower(strings.Trim(f, `"'`+"`.,;:!?"))
	}
	return ""
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

// interruptedRequestRe unwraps the summarization resume prompt —
// agent.go wraps an unfinished turn's prompt as "The previous session
// was interrupted…, the initial user request was: `…`", and selection
// must bind the user's real request, not the harness framing.
var interruptedRequestRe = regexp.MustCompile("(?s)the initial user request was: `(.+)`\\s*$")

// selectOpenFailures is the deterministic task-binding filter between
// ListOpenFailures and tail rendering. Checks run most-explanatory
// first — explicit user scope beats kind binding beats validity — so
// the recorded reason is the strongest single account of why a
// candidate missed the prompt.
//
// Abstention is a valid outcome: an all-rejected set renders no
// <open_failures> section at all, and the decisions record that the
// selector ran and found nothing bound. Two design constraints worth
// stating plainly: under ambiguity only top-scope verification rows
// bind — a legitimately-failing subpackage row never surfaces for "the
// test fails"; and stale_suspect cannot distinguish the agent's own
// in-flight fix (mtime moves as the agent edits) — both read as
// fail-closed by design.
func selectOpenFailures(prompt string, failures []cmdlog.Failure, workDir string) ([]cmdlog.Failure, []FailureDecision) {
	if len(failures) == 0 {
		return nil, nil
	}
	if m := interruptedRequestRe.FindStringSubmatch(prompt); m != nil {
		prompt = m[1]
	}
	pos, neg := promptScope(prompt, workDir)
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
		kind := toolclass.CommandKind(f.Cmd)

		switch {
		case negatedByAny(dirs, neg):
			reason = failNegatedScope
		case explicit && !overlapsAny(dirs, pos):
			reason = failOutOfScope
		case explicit && kind != "test" && kind != "build" && kind != "lint":
			// Explicit scope still requires a verification-flavored
			// row — a failed ls/git/curl at root must not inject into
			// an unrelated "fix main.go".
			reason = failKindMismatch
		case !explicit && kinds == nil:
			reason = failReferentNone
		case !explicit && !slices.Contains(kinds, kind):
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
