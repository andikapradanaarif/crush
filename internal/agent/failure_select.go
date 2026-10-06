package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/toolclass"
)

// FailureDecision is the task-binding selector's verdict on one
// memory candidate — whether it reached the prompt, and if not, why.
// The decision list is recorded on the turn's TailAudit so an eval can
// distinguish "the selector ran and rejected every candidate" from
// "no candidates existed"; it is the audit contract issue #207 asks
// for. Wire mirror: eval.FailureDecision.
type FailureDecision struct {
	// Signature is the candidate's stable id from cmdlog.
	Signature string `json:"signature"`
	// Pool names the memory channel the candidate came from —
	// "open", "resolved", or "command" — so evals can stratify
	// admits and vetoes by the row's semantics, not just its text.
	// Empty on records predating pools; read as "open".
	Pool string `json:"pool,omitempty"`
	// Cmd echoes the candidate command so the record reads without a
	// signature lookup.
	Cmd string `json:"cmd,omitempty"`
	// Admit reports whether the failure rendered into the tail.
	Admit bool `json:"admit"`
	// Reason is a closed vocabulary — the first disqualifying check
	// that fired, "admit", or "render_capped" when the row bound but
	// the render budget cut it. Stable strings: evals and dashboards
	// may group on them.
	Reason string `json:"reason"`
	// SettledBy names the resolver layer that produced Reason — a
	// closed vocabulary ("identifier", "lexicon", "state") so evals
	// can attribute admits and vetoes to the layer that earned them
	// (#216). render_capped rows keep the binding layer here; Reason
	// already records the budget cut.
	SettledBy string `json:"settled_by,omitempty"`
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
	// failNonUserPrompt marks candidates rejected because the prompt
	// was harness-authored retry text, not user intent — the
	// reconcile edge's prompt literally lists open rows, so binding
	// it would admit the very failures it complains about (#249).
	failNonUserPrompt = "non_user_prompt"
	// failLangUnsupported is referent_none when the prompt carries
	// letters the English lexicon cannot parse — a coverage hole, not
	// a clean "no referent". Keeping it a distinct reason makes the
	// hole measurable (#216) instead of invisible inside
	// referent_none.
	failLangUnsupported = "lang_unsupported"
	// failMentionUnknown is referent_none when an identifier was
	// named but its polarity parsed neither way — typed-but-
	// unparseable text. The count is the L3 gate's key quantity:
	// these are the rows a deeper resolver would need to arbitrate.
	// lang_unsupported keeps precedence when both apply — the
	// script hole dominates the mention hole.
	failMentionUnknown = "mention_unknown"
	// failRenderCapped is not a selection check — the row bound, but
	// the render cap cut it. Admit stays false: the field means
	// "rendered into the tail", and this row did not.
	failRenderCapped = "render_capped"
	// failShadowed rejects a command candidate whose (cmd, cwd) key
	// has an open failure row — the warning envelope already names
	// that command, so echoing its ledger twin would render the same
	// knowledge twice. Recorded, not silent: "the command row exists
	// but its open twin carries it" is auditable evidence.
	failShadowed = "shadowed_by_open"
)

// Memory pools — the closed FailureDecision.Pool vocabulary. Open
// failures are warnings; resolved failures and command rows are
// knowledge. All three bind under the same rules; the pool tag is
// what an eval reads to keep their evidence separate.
const (
	poolOpen     = "open"
	poolResolved = "resolved"
	poolCommand  = "command"
)

// Settled-by layers — the closed SettledBy vocabulary. "identifier"
// is the L1 language-neutral mention layer (#216); "lexicon" is the
// English scope/referent checks; "state" is the language-neutral
// validity checks (path_gone, stale_suspect) that no language can
// excuse.
const (
	settledIdentifier = "identifier"
	settledLexicon    = "lexicon"
	settledState      = "state"
	// settledHarness marks verdicts the run-boundary guard settled —
	// the prompt itself was judged non-user text before any layer
	// ran, so no binding layer owns the reason.
	settledHarness = "harness"
)

// verificationKinds are the command kinds a failure referent can
// name — a run row joins them because a crashed program is a verdict
// too ("it panics" describes go run, not go test). "other" never
// binds under ambiguity: a failed ls/git/rm is not what "the bug"
// points at.
var verificationKinds = []string{"test", "build", "lint", "run"}

// referentKindHints maps "the <noun>" referents to the command kinds
// a plausible failure must have. Failure memory can only be the
// referent of a verification-flavored complaint. Generic failure
// nouns span every verdict kind — "the crash" can live in a run row —
// while kind-naming nouns ("the build") stay specific.
var referentKindHints = map[string][]string{
	"test": {"test"}, "tests": {"test"}, "spec": {"test"}, "specs": {"test"},
	"suite": {"test"}, "suites": {"test"},
	"build": {"build"}, "builds": {"build"}, "compile": {"build"},
	"compilation": {"build"},
	"lint":        {"lint"}, "vet": {"lint"}, "linter": {"lint"},
	"typecheck": {"lint"}, "warning": {"lint"}, "warnings": {"lint"},
	"ci": {"test", "build", "lint"}, "check": {"test", "build", "lint"},
	"checks": {"test", "build", "lint"}, "pipeline": {"test", "build", "lint"},
	"pipelines": {"test", "build", "lint"}, "job": {"test", "build", "lint"},
	"jobs": {"test", "build", "lint"},
	"bug":  verificationKinds, "bugs": verificationKinds,
	"bugfix": verificationKinds, "crash": verificationKinds,
	"crashes": verificationKinds, "error": verificationKinds,
	"errors": verificationKinds, "failure": verificationKinds,
	"failures": verificationKinds, "fail": verificationKinds,
	"panic": verificationKinds, "panics": verificationKinds,
	"regression": verificationKinds, "regressions": verificationKinds,
	"leak": verificationKinds, "leaks": verificationKinds,
	"issue": verificationKinds, "issues": verificationKinds,
	"problem": verificationKinds, "problems": verificationKinds,
	"hang": verificationKinds, "hangs": verificationKinds,
	"deadlock": verificationKinds, "deadlocks": verificationKinds,
}

// nonFailureNouns are "the <noun>" referents that claim a
// non-failure target — "update the config", "fix the endpoint". A
// known non-failure noun suppresses the bare-anaphora fallback: in
// "the config is wrong — fix it", "it" points at the config, not at
// any open failure.
var nonFailureNouns = map[string]bool{
	"config": true, "configs": true, "configuration": true,
	"configurations": true, "endpoint": true, "endpoints": true,
	"handler": true, "handlers": true, "route": true, "routes": true,
	"feature": true, "features": true, "change": true, "changes": true,
	"fix": true, "fixes": true, "workaround": true, "workarounds": true,
	"hack": true, "hacks": true, "todo": true, "todos": true,
	"fixme": true, "typo": true, "typos": true, "log": true,
	"logs": true, "doc": true, "docs": true, "readme": true,
	"readmes": true, "ui": true, "cli": true, "api": true,
	"apis": true, "name": true, "names": true, "output": true,
	"outputs": true, "message": true, "messages": true, "text": true,
	"texts": true, "style": true, "styles": true, "color": true,
	"colors": true, "layout": true, "layouts": true, "icon": true,
	"icons": true, "button": true, "buttons": true, "link": true,
	"links": true, "version": true, "versions": true, "schema": true,
	"schemas": true, "flag": true, "flags": true, "option": true,
	"options": true, "comment": true, "comments": true, "file": true,
	"files": true, "format": true, "formats": true, "variable": true,
	"variables": true, "function": true, "functions": true,
	"method": true, "methods": true, "class": true, "classes": true,
	"module": true, "modules": true, "package": true, "packages": true,
	"import": true, "imports": true, "line": true, "lines": true,
	"table": true, "tables": true, "page": true, "pages": true,
	"menu": true, "menus": true, "title": true, "titles": true,
	"image": true, "images": true, "url": true, "urls": true,
	"path": true, "paths": true, "directory": true, "directories": true,
	"folder": true, "folders": true, "setting": true, "settings": true,
	"key": true, "keys": true, "value": true, "values": true,
	"header": true, "headers": true, "dep": true, "deps": true,
	"dependency": true, "dependencies": true, "param": true,
	"params": true, "field": true, "fields": true, "column": true,
	"columns": true, "dialog": true, "dialogs": true, "font": true,
	"fonts": true, "example": true, "examples": true,
}

// anaphoraRe matches the bare pronouns — the referent an open failure
// can claim even when no the-noun spelled one out.
var anaphoraRe = regexp.MustCompile(`(?i)\b(?:it|its|this|that|them|they|these|those)\b`)

// bareReferentKinds applies when the prompt leans on a bare anaphora
// ("fix it", "this is broken") or a failure cue with no noun — an
// ambiguous "fix it" admits any verdict-kind row but never a failed
// ls or deploy; those bind only under explicit scope.
var bareReferentKinds = verificationKinds

// failureCueRe matches failure vocabulary without a definite article
// — "tests are red", "build fails" — so an ambiguous prompt that
// mentions failure at all can bind, while an unrelated request
// ("add a README") rejects every candidate.
var failureCueRe = regexp.MustCompile(`(?i)\b(?:fails?|failed|failing|failures?|broke|broken|breaks?|` +
	`bugs?|crash(?:es|ed|ing)?|panic(?:s|ked|king)?|regress(?:ed|es|ing|ions?)?|red|errors?|erroring|` +
	`flaky|dies|hangs?|deadlocks?|issues?|problems?|leaks?|risky)\b`)

// scopeSlashPathRe matches tokens containing a path separator —
// "decoy/foo.go", "./decoy", "src/pkg/". Matched candidates are gated
// by isScopePath so idioms like "and/or" and "pass/fail" don't mint
// bogus scope — a bare word/word token is not a path.
var scopeSlashPathRe = regexp.MustCompile(`[\w.-]+/[\w./~-]*`)

// scopeFileRe matches bare filenames with a source-ish extension so
// "fix main.go" scopes like "fix decoy/main.go" does.
var scopeFileRe = regexp.MustCompile(`\b[\w-]+\.(?:go|py|rs|ts|tsx|js|jsx|mjs|c|cc|cpp|cxx|h|hpp|` +
	`java|kt|rb|sh|bash|zsh|fish|md|json|ya?ml|toml|mod|sum|sql|proto|css|scss|html|vue|svelte|mk|cfg|ini|` +
	`txt|csv|xml|ipynb|swift|dart)\b`)

// scopeBareWordRe finds candidate tokens for the on-disk directory
// check — "fix decoy" carries no path signal, but if decoy/ exists it
// is scope. Only the disk disambiguates, and grammatical words are
// skipped via scopeStopWords.
var scopeBareWordRe = regexp.MustCompile(`\b[\w][\w-]*\b`)

// scopeStopWords are grammatical and directive words that never mint
// bare-dir scope — articles, pronouns, auxiliaries, and the verbs that
// introduce a target. Nouns that could plausibly be directories
// (test, docs, src) are deliberately absent: if such a dir exists the
// disk check is the truth we have.
var scopeStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true,
	"but": true, "for": true, "with": true, "from": true, "into": true,
	"onto": true, "this": true, "that": true, "them": true, "they": true,
	"these": true, "those": true, "then": true, "when": true,
	"while": true, "where": true, "what": true, "why": true,
	"how": true, "who": true, "are": true, "was": true, "were": true,
	"been": true, "being": true, "not": true, "now": true, "out": true,
	"all": true, "any": true, "some": true, "none": true, "each": true,
	"every": true, "such": true, "same": true, "other": true,
	"another": true, "still": true, "just": true, "only": true,
	"also": true, "too": true, "very": true, "again": true,
	"after": true, "before": true, "there": true, "here": true,
	"please": true, "help": true, "want": true, "need": true,
	"like": true, "have": true, "has": true, "had": true, "can": true,
	"could": true, "should": true, "would": true, "may": true,
	"might": true, "must": true, "shall": true, "will": true,
	"use": true, "see": true, "make": true, "get": true, "got": true,
	"let": true, "way": true, "thing": true, "things": true,
	"something": true, "anything": true, "everything": true,
	"you": true, "your": true, "we": true, "our": true, "me": true,
	"my": true, "him": true, "his": true, "her": true, "its": true,
	"is": true, "it": true, "in": true, "on": true, "at": true,
	"of": true, "to": true, "by": true, "as": true, "be": true,
	"do": true, "does": true, "did": true, "if": true, "so": true,
	"up": true, "down": true, "no": true, "yes": true,
	// Directive verbs — "fix decoy" means decoy is the object, and
	// the verb itself must not mint scope.
	"fix": true, "edit": true, "change": true, "update": true,
	"add": true, "create": true, "remove": true, "delete": true,
	"modify": true, "patch": true, "write": true, "read": true,
	"view": true, "check": true, "handle": true, "work": true,
	"run": true, "build": true, "test": true, "touch": true,
	"implement": true, "correct": true, "repair": true, "address": true,
	"resolve": true, "rework": true, "rewrite": true, "refactor": true,
	"adjust": true, "revert": true, "restore": true, "rebuild": true,
	// Negation vocabulary — "don't touch decoy" needs decoy negated,
	// not the cue words themselves.
	"dont": true, "doesnt": true, "didnt": true, "isnt": true,
	"arent": true, "wasnt": true, "cant": true, "wont": true,
	"couldnt": true, "shouldnt": true, "wouldnt": true, "never": true,
	"avoid": true, "without": true, "skip": true, "except": true,
	"leave": true, "outside": true, "untouched": true, "unmodified": true,
	"keep": true, "keeping": true,
	// Reset conjunctions.
	"however": true, "instead": true, "whereas": true,
	"afterward": true, "afterwards": true,
}

// scopeNegationRe marks a clause as exclusionary — a path after one of
// these cues is scope the user denied, not a target. n't carries no
// leading \b: inside "don't" the n sits mid-word after 'o', so a
// boundary there would never match the most common negation form.
var scopeNegationRe = regexp.MustCompile(`(?i)(?:n't\b|\b(?:not|never|avoid|without|skip|exclud(?:e[sd]?|ing)|except|leave|outside|untouched|unmodified)\b)`)

// scopeResetRe marks hard span boundaries — a reset conjunction ends
// one directive and starts another: "don't touch decoy/, but fix
// main.go" negates decoy only; "fix main.go instead of decoy/x.go"
// re-evaluates decoy/x.go on its own, where it finds no signal.
var scopeResetRe = regexp.MustCompile(`(?i)\b(?:but|however|instead|then|while|whereas|afterwards?|rather)\b`)

// scopeSoftBoundaryRe marks soft span boundaries — punctuation that
// ends a directive segment but lets a signal-free segment inherit the
// governing verdict: "fix main.go, decoy/, and pkg/" keeps the list
// positive.
var scopeSoftBoundaryRe = regexp.MustCompile(`[,;—–]`)

// scopeDirectiveVerbRe marks recognized affirmative signal — a
// directive verb in the token's span declares intent toward it.
// A determiner-led use ("the fix") is a noun phrase, not a
// directive — see determinerLed.
var scopeDirectiveVerbRe = regexp.MustCompile(`(?i)\b(?:fix|change|update|edit|patch|implement|create|add|modify|handle|correct|repair|address|resolve|rework|rewrite|refactor|adjust|revert|build|rebuild|restore|run|test|check|verify|validate|retry|execute|start|restart|touch|keep|open|watch|use|make|see|look|view|inspect|explore|read|review|debug|diagnose|trace|investigate|work)\b`)

// scopeDeterminers make the following word a noun phrase, not a
// directive — "the test" in "don't touch the test in decoy/" is an
// object, not intent.
var scopeDeterminers = map[string]bool{
	"the": true, "a": true, "an": true, "this": true, "that": true,
	"these": true, "those": true, "my": true, "your": true,
	"our": true, "his": true, "her": true, "its": true, "their": true,
}

// lastWordRe captures the final word before a verb match.
var lastWordRe = regexp.MustCompile(`(\w+)\s*$`)

// cmdPathTokenRe matches command arguments that name a repo-relative
// target: "./decoy", ".", "./...", "decoy/", "/abs/path".
var cmdPathTokenRe = regexp.MustCompile(`^(?:\.{1,2}/\S*|\.{3}|\./|\.$|/\S*|[\w.-]+/\S*)$`)

// promptScope splits the prompt's path mentions into positive scope
// (things the user asked to change) and negative scope (things they
// excluded or whose intent could not be established). Polarity is
// span-based: a path mints positive scope only when its span carries
// affirmative signal — a directive verb, a failure cue, or a
// referent — or when the prompt is bare navigation (lonePathPrompt).
// An unrecognized span defaults to exclusion, never to scope: the
// absence of a recognized negation is not evidence of assent, and
// this is the one direction the selector must never fail open —
// "ignore decoy/" and a non-English veto alike suppress rather than
// inject. Two bounded asymmetries follow: "then" is a hard boundary
// ("fix a.go, then b.go" treats b.go as its own signal-free span,
// hence exclusion), and unrecognized action verbs suppress while
// stopword verbs leave bare navigation ("rm decoy/" excludes —
// "rm" is unknown so the span has no signal — but "delete decoy/"
// mints positive because "delete" is a stopword, so nothing but
// the path remains). Both fail toward the safe side for veto
// shapes; the second direction is the price of a stopword lexicon.
func promptScope(prompt, workDir string) (pos, neg []string) {
	lone := lonePathPrompt(prompt)
	for _, c := range splitClauses(prompt) {
		rest := scopeSlashPathRe.ReplaceAllString(c, " ")
		for _, loc := range scopeSlashPathRe.FindAllStringIndex(c, -1) {
			tok := c[loc[0]:loc[1]]
			p := normScopePath(tok)
			if p == "" {
				continue
			}
			negated := !lone && spanNegated(c, loc[0])
			// A negated trailing-slash token mints without path
			// evidence — "don't touch decoy/" is a veto, and a veto
			// that names nothing harms nothing. Positive scope
			// keeps the strict evidence bar.
			if !isScopePath(tok, workDir) &&
				!(negated && (strings.HasSuffix(tok, "/") || strings.HasSuffix(tok, "/."))) {
				continue
			}
			if negated {
				neg = append(neg, p)
			} else {
				pos = append(pos, p)
			}
		}
		for _, loc := range scopeFileRe.FindAllStringIndex(rest, -1) {
			if !lone && spanNegated(rest, loc[0]) {
				neg = append(neg, rest[loc[0]:loc[1]])
			} else {
				pos = append(pos, rest[loc[0]:loc[1]])
			}
		}
		if workDir == "" {
			continue
		}
		// Bare directory names carry no path signal — "fix decoy" —
		// but the disk check disambiguates a real dir from English.
		// Non-failure nouns never mint: "update the config" cannot
		// turn a config/ dir into failure scope, whatever is on disk.
		for _, loc := range scopeBareWordRe.FindAllStringIndex(rest, -1) {
			w := rest[loc[0]:loc[1]]
			lw := strings.ToLower(w)
			if len(lw) < 3 || scopeStopWords[lw] || nonFailureNouns[lw] {
				continue
			}
			if st, err := os.Stat(filepath.Join(workDir, w)); err == nil && st.IsDir() {
				if !lone && spanNegated(rest, loc[0]) {
					neg = append(neg, w)
				} else {
					pos = append(pos, w)
				}
			}
		}
	}
	return dedupeScope(pos), dedupeScope(neg)
}

// spanNegated reports whether the token at start is excluded rather
// than claimed. Its segment runs boundary-to-boundary — soft
// punctuation and hard reset conjunctions both bound it. A negation
// cue anywhere between the last boundary and the token vetoes it —
// "don't even touch decoy/" is negated even though the cue does not
// abut a verb, while "fix decoy/ and skip main.go" negates main.go
// only. With no cue before the token, positive scope must still be
// affirmed by recognized signal anywhere in the segment — object-
// first claims like "main.go is broken" count. A signal-free segment
// inherits the governing verdict across a soft boundary ("fix
// main.go, decoy/" keeps decoy positive) and otherwise excludes.
func spanNegated(c string, start int) bool {
	prefix := c[:start]
	bEnd, bStart, hard := lastScopeBoundary(prefix)
	if scopeNegationRe.MatchString(prefix[bEnd:]) {
		return true
	}
	if spanSignal(c[bEnd:nextScopeBoundary(c, start)]) {
		return false
	}
	if hard || bStart < 0 {
		return true
	}
	return spanNegated(c, bStart)
}

// lastScopeBoundary finds the last span boundary in prefix: a soft
// punctuation mark or a hard reset conjunction, whichever ends
// closest to the token. Returns the boundary's end (where the token's
// span begins), its start (where the previous span ends, for
// inheritance), and whether it is hard. No boundary returns
// start=-1.
func lastScopeBoundary(prefix string) (end, start int, hard bool) {
	end, start = 0, -1
	if m := scopeSoftBoundaryRe.FindAllStringIndex(prefix, -1); m != nil {
		l := m[len(m)-1]
		end, start = l[1], l[0]
	}
	if m := scopeResetRe.FindAllStringIndex(prefix, -1); m != nil {
		if l := m[len(m)-1]; l[1] > end {
			end, start, hard = l[1], l[0], true
		}
	}
	return end, start, hard
}

// nextScopeBoundary finds where the token's segment ends — the first
// boundary at or after start, or end of clause.
func nextScopeBoundary(c string, start int) int {
	end := len(c)
	if m := scopeSoftBoundaryRe.FindStringIndex(c[start:]); m != nil {
		end = start + m[0]
	}
	if m := scopeResetRe.FindStringIndex(c[start:]); m != nil && start+m[0] < end {
		end = start + m[0]
	}
	return end
}

// spanSignal reports whether a segment carries recognized affirmative
// intent — a directive verb (not a determiner-led noun use), a
// failure cue, or a referent. Recognized signal is what affirms
// positive scope; its absence is never evidence of assent.
func spanSignal(seg string) bool {
	for _, v := range scopeDirectiveVerbRe.FindAllStringIndex(seg, -1) {
		if !determinerLed(seg, v[0]) {
			return true
		}
	}
	return vagueReferentRe.MatchString(seg) ||
		failureCueRe.MatchString(seg) ||
		anaphoraRe.MatchString(seg)
}

// determinerLed reports whether the word at offset follows a
// determiner — "the fix" is a noun phrase, not a directive.
func determinerLed(seg string, offset int) bool {
	m := lastWordRe.FindStringSubmatch(strings.TrimRight(seg[:offset], " \t"))
	return m != nil && scopeDeterminers[strings.ToLower(m[1])]
}

// lonePathPrompt reports whether the prompt is bare navigation — only
// path tokens, stopwords, and punctuation. A bare path is affirmative
// by convention: nobody types a path alone to veto it, and there is
// no unrecognized text to misparse. A negation cue anywhere still
// disqualifies.
func lonePathPrompt(prompt string) bool {
	rest := scopeSlashPathRe.ReplaceAllString(prompt, " ")
	rest = scopeFileRe.ReplaceAllString(rest, " ")
	// A negation cue or a boundary conjunction means real syntax to
	// parse — "fix main.go instead of decoy/x.go" is not bare
	// navigation even though its words are all stopwords.
	if scopeNegationRe.MatchString(rest) || scopeResetRe.MatchString(rest) {
		return false
	}
	for _, w := range strings.Fields(rest) {
		w = strings.Trim(w, " \t.,;:!?()[]{}\"'`—–-")
		if w != "" && !scopeStopWords[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// identRe extracts identifier-shaped tokens — the language-neutral
// surface vocabulary a headline or a prompt can carry ("TestAdd",
// "test_add"). Shape alone is not proof: isIdentifier filters out
// ordinary English words before a token counts as an identifier.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// isIdentifier reports whether a token is code-shaped rather than
// English-shaped: it needs a lowercase letter plus an underscore or
// an uppercase letter past the first position ("TestAdd", "test_add",
// "AssertionError"). All-caps words ("FAIL") and plain lowercase
// words ("panic", "runtime") fail — a headline's English is not a
// mention vocabulary, and neither is a prompt's.
func isIdentifier(tok string) bool {
	hasLower, hasSignal := false, false
	for i, r := range tok {
		switch {
		case r == '_':
			hasSignal = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r) && i > 0:
			hasSignal = true
		}
	}
	return hasLower && hasSignal
}

// headlineIdentifiers is the row's language-neutral mention
// vocabulary: the identifier-shaped tokens its recorded headline
// carries. "--- FAIL: TestAdd (0.00s)" yields {TestAdd}; subtest
// segments yield their own entries, so "TestAdd/sub_1" binds on
// either name. This is the L1 answer to the headline-binding gap —
// "fix TestAdd" names the row in any language.
func headlineIdentifiers(f cmdlog.Failure) map[string]bool {
	ids := map[string]bool{}
	for _, tok := range identRe.FindAllString(f.Headline, -1) {
		if isIdentifier(tok) {
			ids[tok] = true
		}
	}
	return ids
}

// identSite is one identifier mention in the prompt — the clause and
// byte offset where it appeared, so its polarity resolves against its
// own span.
type identSite struct {
	clause string
	start  int
}

// promptIdentSites indexes identifier-shaped prompt tokens by token.
// Extraction is symmetric with headlineIdentifiers: the same shape
// rule applies, so English words cannot mint a mention of nothing.
func promptIdentSites(prompt string) map[string][]identSite {
	sites := map[string][]identSite{}
	for _, c := range splitClauses(prompt) {
		for _, loc := range identRe.FindAllStringIndex(c, -1) {
			if tok := c[loc[0]:loc[1]]; isIdentifier(tok) {
				sites[tok] = append(sites[tok], identSite{clause: c, start: loc[0]})
			}
		}
	}
	return sites
}

// mentionPolarity is a mention's three-valued verdict for one
// candidate. Veto means a site's span carried a recognized negation
// ("don't touch TestAdd"); positive means recognized signal or bare
// navigation ("fix TestAdd", "TestAdd is broken", a lone identifier);
// unknown means the token was named but its span parsed neither way —
// typed-but-unparseable text, a non-English polarity. Unknown never
// binds and never vetoes at this layer: the mention is real, so the
// row is *mentioned* — L2 cannot resurrect it — and the unresolved
// polarity is what L3/L4 exist for.
type mentionPolarity int

const (
	mentionNone mentionPolarity = iota
	mentionVeto
	mentionPositive
	mentionUnknown
)

// mentionPolarityAt resolves one mention site's polarity — negation
// cue in its segment vetoes, recognized signal affirms, and a
// signal-free segment after only a soft boundary inherits the
// governing span's verdict (same rule paths follow: "fix main.go,
// TestAdd" keeps TestAdd positive). A signal-free root or hard-bounded
// segment is unknown, not vetoed: the absence of a recognized negation
// is not a veto, and the absence of signal is not assent.
func mentionPolarityAt(c string, start int, lone bool) mentionPolarity {
	prefix := c[:start]
	bEnd, bStart, hard := lastScopeBoundary(prefix)
	if scopeNegationRe.MatchString(prefix[bEnd:]) {
		return mentionVeto
	}
	if lone || spanSignal(c[bEnd:nextScopeBoundary(c, start)]) {
		return mentionPositive
	}
	if !hard && bStart >= 0 {
		return mentionPolarityAt(c, bStart, false)
	}
	return mentionUnknown
}

// candidateMention folds every mention site naming one of the
// candidate's identifiers into a single polarity. A veto anywhere
// wins over positive mentions ("don't touch TestAdd — fix TestOther"
// vetoes TestAdd even if a later span is positive); unknown sites
// only matter when no recognized polarity exists.
func candidateMention(sites map[string][]identSite, ids map[string]bool, lone bool) mentionPolarity {
	pol := mentionNone
	for tok, ss := range sites {
		if !ids[tok] {
			continue
		}
		for _, s := range ss {
			switch p := mentionPolarityAt(s.clause, s.start, lone); p {
			case mentionVeto:
				return mentionVeto
			case mentionPositive:
				pol = mentionPositive
			case mentionUnknown:
				if pol == mentionNone {
					pol = mentionUnknown
				}
			}
		}
	}
	return pol
}

// loneIdentPrompt reports whether the prompt is bare identifier
// navigation — the lonePathPrompt convention extended to L1: nobody
// types "TestAdd" alone to veto it, so a lone identifier is an
// affirmative mention. Negation cues and reset conjunctions still
// disqualify.
func loneIdentPrompt(prompt string) bool {
	rest := scopeSlashPathRe.ReplaceAllString(prompt, " ")
	rest = scopeFileRe.ReplaceAllString(rest, " ")
	rest = identRe.ReplaceAllStringFunc(rest, func(tok string) string {
		if isIdentifier(tok) {
			return " "
		}
		return tok
	})
	if scopeNegationRe.MatchString(rest) || scopeResetRe.MatchString(rest) {
		return false
	}
	for _, w := range strings.Fields(rest) {
		w = strings.Trim(w, " \t.,;:!?()[]{}\"'`—–-")
		if w != "" && !scopeStopWords[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// nonASCIILetter reports whether the prompt carries letters outside
// the lexicon's English — CJK, accented Latin, Cyrillic. The lexicon
// cannot parse them, so a referent_none there is a coverage hole the
// selector must measure (lang_unsupported), not a clean absence.
func nonASCIILetter(s string) bool {
	for _, r := range s {
		if r > 0x7F && unicode.IsLetter(r) {
			return true
		}
	}
	return false
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
	// A dotted first segment reads as a host — "github.com/x/y" is a
	// URL, not a repo path — unless it exists on disk ("v1.0/dir/").
	if strings.Contains(tok[:strings.IndexByte(tok, '/')], ".") {
		if workDir != "" {
			if _, err := os.Stat(filepath.Join(workDir, normScopePath(tok))); err == nil {
				return true
			}
		}
		return false
	}
	if strings.HasSuffix(tok, "/...") {
		return true
	}
	if strings.HasSuffix(tok, "/") || strings.HasSuffix(tok, "/.") {
		// A bare trailing slash is a weak signal — "w/" is English
		// for "with", not a directory. It mints scope only with a
		// second segment ("src/decoy/") or a disk hit; negated
		// single-segment forms ("don't touch decoy/") mint via the
		// veto exception in promptScope.
		if strings.Count(tok, "/") >= 2 {
			return true
		}
		if workDir != "" {
			if _, err := os.Stat(filepath.Join(workDir, normScopePath(tok))); err == nil {
				return true
			}
		}
		return false
	}
	if strings.Count(tok, "/") >= 2 {
		// Multi-slash idioms ("pass/fail/skip", "on/off/auto") are
		// enumerations, not paths — a token whose every segment is
		// an idiom word names options, not directories.
		allIdioms := true
		for _, s := range strings.Split(strings.TrimSuffix(tok, "/"), "/") {
			if !scopeIdiomWords[s] {
				allIdioms = false
				break
			}
		}
		return !allIdioms
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
			(r == '.' && (i == 0 || prompt[i-1] != '.') && (i+1 == len(prompt) || prompt[i+1] == ' ' || prompt[i+1] == '\t'))
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
// constraint — and for paths that climb above the root: "../sibling"
// has no expressible repo scope and must not collapse into one.
func normScopePath(p string) string {
	p = strings.Trim(p, `"'`+"`")
	depth := 0
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "..":
			depth--
		case "", ".":
		default:
			depth++
		}
		if depth < 0 {
			return ""
		}
	}
	// A Go recursive tail binds the parent dir — "decoy/..." is decoy.
	p = strings.TrimSuffix(p, "/...")
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

// scopeIdiomWords are the enumeration words multi-slash idioms draw
// from — kept narrow on purpose: "src/api/v2" is a path because "src"
// isn't here, while "pass/fail/skip" is an enumeration because every
// segment is.
var scopeIdiomWords = map[string]bool{
	"and": true, "or": true, "pass": true, "fail": true, "skip": true,
	"read": true, "write": true, "input": true, "output": true,
	"yes": true, "no": true, "true": true, "false": true,
	"on": true, "off": true, "in": true, "out": true, "up": true,
	"down": true, "left": true, "right": true, "enable": true,
	"disable": true, "none": true, "all": true, "auto": true,
}

// dirWithinNegated is the directional version for exclusions: a
// candidate is bound to an excluded path when its scope sits inside
// it ("go test ./decoy" inside "do not touch decoy"), NOT when it
// merely covers it — a root-scope command covers every excluded path
// without being bound to it. A file-form exclusion ("don't touch
// decoy/gen.go") binds rows scoped to the file's directory, but
// yields when the same dir is positively scoped — "don't touch
// decoy/gen.go but fix decoy/handler.go" is resolved by the positive
// half, not vetoed by the negative.
func dirWithinNegated(dir, p string, pos []string) bool {
	p = strings.TrimSuffix(p, "/")
	if dir == p || strings.HasPrefix(dir, p+"/") {
		return true
	}
	if !scopeFileRe.MatchString(path.Base(p)) {
		return false
	}
	d := path.Dir(p)
	if d == "." || (dir != d && !strings.HasPrefix(dir, d+"/")) {
		return false
	}
	for _, q := range pos {
		if pathsOverlap(d, q) {
			return false
		}
	}
	return true
}

// cmdValueFlags take their value as a separate token — the value is
// not a target, so it never enters binding scope ("-o ./bin/t" mints
// no bin/ dir). Flags spelled -flag=value skip as flag tokens anyway.
var cmdValueFlags = map[string]bool{
	"-o": true, "-exec": true, "-cpuprofile": true, "-memprofile": true,
	"-mutexprofile": true, "-blockprofile": true, "-trace": true,
	"-coverprofile": true, "-coverpkg": true, "-outputdir": true,
	"-modfile": true, "-overlay": true, "-pkgdir": true, "-toolexec": true,
	"-ldflags": true, "-gcflags": true, "-asmflags": true, "-tags": true,
	"-run": true, "-bench": true, "-benchtime": true, "-skip": true,
	"-count": true, "-timeout": true, "-parallel": true, "-cpu": true,
	"-covermode": true, "-work": true,
}

// cmdDirFlags carry the command's working directory as their value —
// "make -C decoy" runs in decoy, so the flag's argument is the scope,
// not an opaque operand. Only unambiguous dir flags qualify: -d/-f
// toggle boolean modes on most tools (rm -f x.go must not eat x.go
// as a directory), so they stay with the composite limitation rather
// than guess per-tool.
var cmdDirFlags = map[string]bool{
	"-C": true, "--directory": true, "--dir": true,
	"--working-directory": true, "--prefix": true,
}

// shellSepRe marks shell separators — tokens after them belong to the
// next composite segment.
var shellSepRe = regexp.MustCompile(`^(?:&&|\|\||[;|&])$`)

// cmdTargets extracts the command's repo-relative target args.
// "go test -count=1 ./decoy" → ["decoy"]; "go test ." → ["."]; a Go
// recursive pattern sheds its "/..." tail to the parent dir ("go test
// ./decoy/..." → "decoy"); a dir-flag value is scope too ("make -C
// decoy" → ["decoy"], the same folded-CWD class) and becomes the
// effective CWD for later relative targets ("go -C decoy test ." →
// ["decoy"], not root); a command with no path args returns nil — its
// scope is its CWD. Quoted single-word args ("./decoy") unwrap.
// Composite commands mint scope from every segment — "go test . &&
// rm -rf decoy/" binds both "." and "decoy" — because cmdlog keeps no
// per-segment argv; under negation that over-rejects, under explicit
// scope it over-admits. Known distortion, same resolution limit the
// ledger itself has.
func cmdTargets(cmd string) []string {
	var out []string
	skipValue := false
	dirValue := false
	skipArgs := false
	base := "" // Dir-flag effective CWD for later relative targets.
	mint := func(t string) {
		t = strings.TrimPrefix(t, "./")
		t = strings.TrimSuffix(t, "/...")
		if scopeFileRe.MatchString(path.Base(t)) {
			t = path.Dir(t)
		}
		if t == "..." || t == "" {
			t = "."
		}
		if base != "" && !path.IsAbs(t) && !strings.HasPrefix(t, "..") {
			t = path.Join(base, t)
		}
		out = append(out, path.Clean(t))
	}
	for _, tok := range strings.Fields(cmd) {
		if skipArgs {
			// Everything after -args belongs to the test binary —
			// until the next segment starts.
			skipArgs = !shellSepRe.MatchString(tok)
			continue
		}
		if skipValue {
			skipValue = false
			continue
		}
		tok = strings.Trim(tok, `"'`)
		if dirValue {
			dirValue = false
			base = joinCmdBase(base, tok)
			out = append(out, base)
			continue
		}
		if tok == "-args" {
			skipArgs = true
			continue
		}
		if strings.HasPrefix(tok, "-") {
			name, val := tok, ""
			hasInline := false
			if i := strings.IndexByte(tok, '='); i >= 0 {
				name, val, hasInline = tok[:i], tok[i+1:], true
			}
			skipValue = cmdValueFlags[name] && !hasInline
			dirValue = cmdDirFlags[name] && !hasInline
			switch {
			case cmdDirFlags[name] && hasInline:
				base = joinCmdBase(base, val)
				out = append(out, base)
			case strings.HasPrefix(tok, "-C") && len(tok) > 2:
				// -Cdecoy — joined short-flag spelling (make, git).
				base = joinCmdBase(base, tok[2:])
				out = append(out, base)
			}
			continue
		}
		if !cmdPathTokenRe.MatchString(tok) {
			continue
		}
		mint(tok)
	}
	return out
}

// joinCmdBase folds a dir-flag value into the command's effective
// working directory — repeated -C flags nest ("make -C a -C b" runs
// in a/b), and an absolute value starts over.
func joinCmdBase(base, val string) string {
	val = strings.Trim(val, `"'`)
	v := strings.TrimSuffix(strings.TrimPrefix(val, "./"), "/")
	if path.IsAbs(v) {
		return path.Clean(v)
	}
	if v == "" || v == "." {
		return base
	}
	if base == "" || base == "." {
		return path.Clean(v)
	}
	return path.Join(base, v)
}

// relCWD normalizes a recorded CWD to workspace-relative form — rows
// store workspace-relative cwds already ("." for root), so the
// filepath.Rel call only fires for legacy absolute values; "top-level"
// means the workdir itself. A cwd outside the workdir stays non-root:
// it cannot bind to repo scope.
func relCWD(cwd, workDir string) string {
	if cwd == "" || cwd == "." {
		return "."
	}
	if workDir != "" {
		if rel, err := filepath.Rel(workDir, cwd); err == nil {
			// ".." and "../…" escape the workdir; a leading-dot name
			// like "..foo" is a real dir inside it, not an escape.
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.ToSlash(rel)
			}
			return cwd // outside the workdir — never repo-root scope.
		}
	}
	return cwd
}

// resolveTarget maps one command target to the row's binding dir.
// Relative targets resolve against the recorded CWD — "cd decoy &&
// go test ." is decoy-scoped, not root-scoped; a "." that fell out of
// "./..." or "." is only root when the CWD is root. An absolute target
// inside the workdir relativizes; one outside stays absolute and can
// never bind repo scope.
func resolveTarget(target, cwd, workDir string) string {
	if filepath.IsAbs(target) {
		if workDir != "" {
			if rel, err := filepath.Rel(workDir, target); err == nil &&
				rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return rel
			}
		}
		return target
	}
	return path.Join(cwd, target)
}

// failureDirs is the candidate's binding scope: the dirs its command
// targets resolve to (against its CWD) plus the dirs its implicated
// files live in. File hints are stored workspace-relative, so they
// join scope unmodified. CWD stands alone only when nothing narrower
// identifies scope — a command run from root that names decoy/ is
// decoy-scoped, not root-scoped.
func failureDirs(f cmdlog.Failure, workDir string) []string {
	cwd := relCWD(f.CWD, workDir)
	var dirs []string
	for _, t := range cmdTargets(f.Cmd) {
		dirs = append(dirs, resolveTarget(t, cwd, workDir))
	}
	for _, file := range f.Files {
		dirs = append(dirs, path.Dir(file))
	}
	if len(dirs) == 0 {
		return []string{cwd}
	}
	return dedupeScope(dirs)
}

// failureTopLevel reports whether the candidate's command runs at
// project scope — a target resolving to the workdir root (".", "./..."
// at root, or "." under a root CWD) or no path args at all with a root
// CWD. A subpath-bound row under an ambiguous prompt is a
// wrong-referent risk: it anchors the agent to a corner while the
// declared referent usually lives at top scope.
//
// Deliberately narrower than failureDirs: file hints widen a row's
// binding scope for exclusion/overlap purposes, but hints in sub/
// don't mean the command was scoped to sub/ — "go test" at root whose
// output names sub/x_test.go is still a top-level run. The two read
// different signals: top-levelness is where the command ran, scope is
// everything the failure implicates.
func failureTopLevel(f cmdlog.Failure, workDir string) bool {
	cwd := relCWD(f.CWD, workDir)
	targets := cmdTargets(f.Cmd)
	if len(targets) == 0 {
		return cwd == "."
	}
	for _, t := range targets {
		if resolveTarget(t, cwd, workDir) == "." {
			return true
		}
	}
	return false
}

// referentKinds resolves the ambiguous prompt's referents to the
// command kinds an open failure may claim, unioning across every
// "the <noun>" in the prompt — an adjective capture ("the failing
// test" → "failing") falls through to the next word, and a
// non-failure noun never disqualifies a later failure noun. Nil
// means no the-noun bound a kind: either the prompt names targets
// failure memory cannot supply ("the config"), or it never mentions
// failure at all. Asymmetry worth stating: "the server is broken"
// rejects (the article claims a specific referent the hints can't
// map) while "my server is broken" admits — only a bare anaphora
// rescues an unrecognized the-noun, so the definite article is the
// more restrictive construction unless the noun is a known
// non-failure word, in which case even a pronoun can't rescue it.
func referentKinds(prompt string) []string {
	if !vagueReferentRe.MatchString(prompt) && !failureCueRe.MatchString(prompt) {
		return nil
	}
	matches := theNounRe.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		return bareReferentKinds
	}
	var kinds []string
	claimedNonFailure := false
	for _, m := range matches {
		noun := strings.ToLower(prompt[m[2]:m[3]])
		cands := referentKindHints[noun]
		if cands == nil && nonFailureNouns[noun] {
			claimedNonFailure = true
			continue
		}
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
	// Bare-anaphora fallback: "the <unknown> — fix them" has a pronoun
	// a bound failure could claim, unless a known non-failure noun
	// already claimed it.
	if len(kinds) == 0 && !claimedNonFailure && anaphoraRe.MatchString(prompt) {
		return bareReferentKinds
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
// workdir paths. Captured hints are stored workspace-relative, but
// rows written before that normalization — or hints that were only
// package-relative — still resolve when tried under the run's CWD and
// each resolved target dir, so all joins are probed.
func failurePaths(f cmdlog.Failure, workDir string) []string {
	var out []string
	var bases []string
	cwd := relCWD(f.CWD, workDir)
	if cwd != "." {
		bases = append(bases, cwd)
	}
	for _, t := range cmdTargets(f.Cmd) {
		if t != "." {
			bases = append(bases, resolveTarget(t, cwd, workDir))
		}
	}
	bases = append(bases, "")
	for _, file := range f.Files {
		if filepath.IsAbs(file) {
			out = append(out, file)
			continue
		}
		for _, b := range bases {
			if filepath.IsAbs(b) {
				out = append(out, filepath.Join(b, file))
				continue
			}
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

// selectorPromptReplacer folds typographic variants into the forms
// the parser knows: smart apostrophes to ASCII (don't vs don't —
// a miss would silently invert negation to positive scope) and
// backticks out ("the `tests` are failing" binds like "the tests").
var selectorPromptReplacer = strings.NewReplacer(
	"‘", "'", "’", "'", "`", "",
)

// selectOpenFailures is the deterministic task-binding filter between
// ListOpenFailures and tail rendering. Checks run most-explanatory
// first — explicit user scope beats kind binding beats validity — so
// the recorded reason is the strongest single account of why a
// candidate missed the prompt.
//
// Abstention is a valid outcome: an all-rejected set renders no
// <open_failures> section at all, and the decisions record that the
// selector ran and found nothing bound. Design constraints stated
// plainly:
//
//   - Polarity is affirmed, never assumed. Positive scope requires
//     recognized signal in the token's span — a directive verb, a
//     failure cue, a referent, or a bare-path prompt. A negation cue
//     vetoes; everything else defaults to exclusion. Unparseable
//     text — an unfamiliar English veto, a non-English prompt —
//     therefore suppresses rather than scopes: the lexicon may only
//     grant, never assent by absence.
//   - Under ambiguity only top-scope verification rows bind — a
//     legitimately-failing subpackage row never surfaces for "the
//     test fails". The run-kind carve-out covers cwd-bound runs
//     too ("cd decoy && npm start"): a run target names the
//     binary's package, not the panic's scope.
//   - Explicit scope means "the prompt named paths", not "the
//     prompt is about a failure" — "add a README.md" still admits an
//     overlapping root `go test .` row, because a failing root check
//     is global signal; scope decides overlap, not relevance.
//   - stale_suspect is an mtime proxy for content change — a
//     formatter, go generate, git checkout/pull, or an unrelated
//     edit to a hinted file suppresses the row until re-observed,
//     including the agent's own in-flight fix. Fail-closed by
//     design: the row hides rather than anchors.
//   - Exclusion strength is asymmetric: a dir-form exclusion
//     ("don't touch decoy/") always binds rows inside it, while a
//     file-form exclusion binds the file's directory and everything
//     under it but yields to same-dir positive scope.
//   - An empty prompt (attachment-only turns) binds nothing —
//     referent_none, consistent fail-closed.
//   - Headline identifiers bind through the L1 mention layer —
//     "fix TestAdd" matches a row whose recorded headline names
//     TestAdd, in any surrounding language, because the token's own
//     span still has to carry recognized signal (a directive verb, a
//     cue, or bare navigation). A mention whose span parses neither
//     way neither binds nor vetoes: it is recorded as a mention and
//     left for deeper layers (#216's L3/L4). cmdNorm truncates at
//     500 runes and quoted multi-word paths split at the space —
//     both bounded.
//   - Prompts the English lexicon cannot parse report
//     lang_unsupported instead of referent_none, so the coverage
//     hole is measured rather than silent.
//   - A mention whose polarity parses neither way neither binds nor
//     vetoes. When nothing else decides the row it records
//     mention_unknown (settled=identifier) — the unresolvable-
//     mention count the L3 gate watches — rather than a bare
//     referent_none. lang_unsupported keeps precedence on
//     non-ASCII prompts: the script hole dominates the mention
//     hole. On rows other evidence settles (an admit, a path
//     veto) the causal reason stands and the unknown mention is
//     not separately marked.
//
// renderLimit is the render stage's budget, applied after selection:
// the freshest renderLimit bound rows render (input order is
// recent-first), and every bound row beyond it keeps a decision with
// render_capped — cut by budget, not by the prompt. renderLimit <= 0
// renders everything that binds.
//
// selectOpenFailures is the test-facing open-pool-only wrapper over
// selectMemory — production calls selectMemory directly so all three
// pools share one prompt analysis. A new caller wanting open-only
// verdicts should bind pools explicitly, not reach for this shape.
func selectOpenFailures(prompt string, failures []cmdlog.Failure, workDir string,
	renderLimit int) ([]cmdlog.Failure, []FailureDecision) {
	admitted, decisions := selectMemory(prompt,
		memoryPools{open: failures}, workDir, memoryRenderLimits{open: renderLimit})
	return admitted.open, decisions
}

// memoryPools groups the three memory channels the selector sees in
// one pass — open failures, resolved failures, and the command
// ledger. One prompt analysis binds all three, so a row cannot admit
// under a reading its sibling rejected; the pool tag on each
// decision keeps the channels' evidence separable downstream.
type memoryPools struct {
	open     []cmdlog.Failure
	resolved []cmdlog.Failure
	commands []cmdlog.Command
}

// memoryRenderLimits caps rendered rows per pool — the same
// after-selection budget selectOpenFailures documents, applied
// independently so each pool's render_capped verdicts are honest.
// A non-positive limit renders everything that binds.
type memoryRenderLimits struct {
	open     int
	resolved int
	command  int
}

// selCandidate normalizes one memory row to the selector's view.
// Command rows wear a Failure-shaped view (signature synthesized
// from the row's key) so the shared binding rules apply unchanged;
// the cmd pointer maps an admit back to the ledger row. shadowed
// marks a command row whose key an open failure already carries.
type selCandidate struct {
	pool     string
	f        cmdlog.Failure
	cmd      *cmdlog.Command
	ids      map[string]bool
	shadowed bool
}

// commandCandidateID is a command row's stable id for decisions —
// the row's primary key is (cmd_norm, cwd), so the digest of that
// pair plays the role failure signatures play.
func commandCandidateID(c cmdlog.Command) string {
	sum := sha256.Sum256([]byte(c.CmdNorm + "\x00" + c.CWD))
	return hex.EncodeToString(sum[:8])
}

// commandCandidateViews maps ledger rows to the Failure-shaped view
// telemetry counts candidates by — LastSeen carries the row's LastAt
// observation so age stats stay in the same units as failure rows.
func commandCandidateViews(commands []cmdlog.Command) []cmdlog.Failure {
	out := make([]cmdlog.Failure, 0, len(commands))
	for _, c := range commands {
		out = append(out, cmdlog.Failure{
			Signature: commandCandidateID(c),
			Cmd:       c.CmdNorm,
			CWD:       c.CWD,
			LastSeen:  c.LastAt,
		})
	}
	return out
}

// commandIdentifiers is a command row's mention vocabulary: the
// identifier-shaped tokens its recorded command carries. Unlike a
// failure row — whose cmd is the invocation and whose headline is
// the identity — a command row's content IS the command, so its
// tokens are what a named mention can bind.
func commandIdentifiers(c cmdlog.Command) map[string]bool {
	ids := map[string]bool{}
	for _, tok := range identRe.FindAllString(c.CmdNorm, -1) {
		if isIdentifier(tok) {
			ids[tok] = true
		}
	}
	return ids
}

func selectMemory(prompt string, pools memoryPools, workDir string,
	limits memoryRenderLimits) (memoryPools, []FailureDecision) {
	var admitted memoryPools
	total := len(pools.open) + len(pools.resolved) + len(pools.commands)
	if total == 0 {
		return admitted, nil
	}
	candidates := make([]selCandidate, 0, total)
	openKeys := make(map[string]bool, len(pools.open))
	for _, f := range pools.open {
		candidates = append(candidates, selCandidate{pool: poolOpen, f: f})
		openKeys[f.Cmd+"\x00"+strings.ReplaceAll(f.CWD, `\`, "/")] = true
	}
	for _, f := range pools.resolved {
		candidates = append(candidates, selCandidate{pool: poolResolved, f: f})
	}
	for i := range pools.commands {
		c := &pools.commands[i]
		candidates = append(candidates, selCandidate{
			pool: poolCommand, cmd: c,
			f: cmdlog.Failure{
				Signature: commandCandidateID(*c),
				Cmd:       c.CmdNorm,
				CWD:       c.CWD,
			},
			ids:      commandIdentifiers(*c),
			shadowed: openKeys[c.CmdNorm+"\x00"+strings.ReplaceAll(c.CWD, `\`, "/")],
		})
	}
	if strings.Contains(prompt, reconcileRetryPrefix) {
		// The reconcile edge's retry prompt names the rows it flags.
		// The once-per-turn cache in turnTailMessages keeps retry
		// text away from selection; this guard keeps the selector
		// honest if a future caller ever hands it harness text.
		ds := make([]FailureDecision, 0, len(candidates))
		for _, c := range candidates {
			ds = append(ds, FailureDecision{
				Signature: c.f.Signature, Cmd: c.f.Cmd, Pool: c.pool,
				Reason: failNonUserPrompt, SettledBy: settledHarness,
			})
		}
		return admitted, ds
	}
	openLimit, resolvedLimit, commandLimit := limits.open, limits.resolved, limits.command
	if openLimit <= 0 {
		openLimit = len(pools.open)
	}
	if resolvedLimit <= 0 {
		resolvedLimit = len(pools.resolved)
	}
	if commandLimit <= 0 {
		commandLimit = len(pools.commands)
	}
	if m := interruptedRequestRe.FindStringSubmatch(prompt); m != nil {
		prompt = m[1]
	}
	// Windows-native strings normalize by literal replacement —
	// filepath.ToSlash is a no-op off Windows, and rows produced on
	// Windows must still bind everywhere.
	prompt = strings.ReplaceAll(selectorPromptReplacer.Replace(prompt), `\`, "/")
	pos, neg := promptScope(prompt, workDir)
	explicit := len(pos) > 0
	var kinds []string
	if !explicit {
		kinds = referentKinds(prompt)
	}
	// L1: language-neutral identifier mentions — the same scan once,
	// folded per candidate below. lone is bare-identifier navigation
	// only: a prompt carrying an identifier site can never satisfy
	// lonePathPrompt (the token survives its strips), so it adds
	// nothing here.
	identSites := promptIdentSites(prompt)
	lone := loneIdentPrompt(prompt)

	decisions := make([]FailureDecision, 0, len(candidates))
	for _, cand := range candidates {
		f := cand.f
		// Windows rows arrive OS-native — normalize to slashes so
		// dir/prefix comparisons work in the separator the path
		// helpers assume. Clone before rewriting in place: the
		// slice shares the caller's backing array.
		f.CWD = strings.ReplaceAll(f.CWD, `\`, "/")
		f.Files = slices.Clone(f.Files)
		for i := range f.Files {
			f.Files[i] = strings.ReplaceAll(f.Files[i], `\`, "/")
		}
		d := FailureDecision{Signature: f.Signature, Cmd: f.Cmd, Pool: cand.pool,
			SettledBy: settledLexicon}
		reason := failAdmit
		dirs := failureDirs(f, workDir)
		kind := toolclass.CommandKind(f.Cmd)
		ids := cand.ids
		if ids == nil {
			ids = headlineIdentifiers(f)
		}
		mention := candidateMention(identSites, ids, lone)

		switch {
		case cand.shadowed:
			// The shadow precedes every other check: the open twin's
			// own decision carries the real verdict on this command,
			// and this row's verdict is "subsumed", not rejected on
			// the merits.
			reason = failShadowed
			d.SettledBy = settledState
		case negatedByAny(dirs, f.Files, neg, pos):
			reason = failNegatedScope
		case mention == mentionVeto:
			// A named identifier inside a negated span vetoes its
			// own row — path-veto parity at the mention layer.
			reason = failNegatedScope
			d.SettledBy = settledIdentifier
		case mention == mentionPositive && !slices.Contains(verificationKinds, kind):
			// A named identifier binds a row the way explicit scope
			// does — and still requires a verdict-flavored row: a
			// failed ls whose headline happens to carry the token
			// must not inject.
			reason = failKindMismatch
			d.SettledBy = settledIdentifier
		case mention == mentionPositive && allPathsGone(f, workDir):
			reason = failPathGone
			d.SettledBy = settledState
		case mention == mentionPositive && anyPathNewer(f, workDir):
			reason = failStaleSuspect
			d.SettledBy = settledState
		case mention == mentionPositive:
			// A positively-mentioned identifier binds regardless of
			// dir scope or top-levelness — the mention IS the
			// scope. Polarity was resolved language-neutrally (cue,
			// verb, or lone), so non-English positive prompts only
			// bind here when the English signal is genuinely
			// present.
			d.SettledBy = settledIdentifier
		case explicit && !overlapsAny(dirs, pos):
			reason = failOutOfScope
		case explicit && !slices.Contains(verificationKinds, kind):
			// Explicit scope still requires a verdict-flavored row —
			// a failed ls/git/curl at root must not inject into an
			// unrelated "fix main.go".
			reason = failKindMismatch
		case !explicit && kinds == nil:
			switch {
			case nonASCIILetter(prompt):
				// The lexicon could not parse the prompt — a
				// measured coverage hole, not a clean referent
				// absence.
				reason = failLangUnsupported
			case mention == mentionUnknown:
				// An identifier was named but its polarity parsed
				// neither way. Nothing else bound, so the record
				// marks the unresolvable mention rather than a
				// bare referent absence.
				reason = failMentionUnknown
				d.SettledBy = settledIdentifier
			default:
				reason = failReferentNone
			}
		case !explicit && !slices.Contains(kinds, kind):
			reason = failKindMismatch
		case !explicit && kind != "run" && !failureTopLevel(f, workDir):
			// A run row's subdir target names the binary's package,
			// not the failure's scope — the panic can live anywhere
			// in its call graph — so narrow_scope is a verification-
			// kind rule only.
			reason = failNarrowScope
		case allPathsGone(f, workDir):
			reason = failPathGone
			d.SettledBy = settledState
		case anyPathNewer(f, workDir):
			reason = failStaleSuspect
			d.SettledBy = settledState
		}
		d.Admit = reason == failAdmit
		limit := openLimit
		switch cand.pool {
		case poolResolved:
			limit = resolvedLimit
		case poolCommand:
			limit = commandLimit
		}
		var poolCount int
		switch cand.pool {
		case poolResolved:
			poolCount = len(admitted.resolved)
		case poolCommand:
			poolCount = len(admitted.commands)
		default:
			poolCount = len(admitted.open)
		}
		if d.Admit && poolCount >= limit {
			d.Admit = false
			d.Reason = failRenderCapped
		} else {
			d.Reason = reason
		}
		decisions = append(decisions, d)
		if d.Admit {
			switch cand.pool {
			case poolResolved:
				admitted.resolved = append(admitted.resolved, f)
			case poolCommand:
				admitted.commands = append(admitted.commands, *cand.cmd)
			default:
				admitted.open = append(admitted.open, f)
			}
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

func negatedByAny(dirs, files, neg, pos []string) bool {
	for _, d := range dirs {
		for _, p := range neg {
			if dirWithinNegated(d, p, pos) {
				return true
			}
		}
	}
	// A file-form veto at root cannot bind through dir containment —
	// every dir sits under "." — so it binds the implicated file
	// itself: "don't touch main_test.go — fix decoy/x.go" vetoes a
	// row whose Files name main_test.go. It yields only to positive
	// scope in that same (root) dir — "fix main.go" resolves toward
	// the file's neighbors.
	for _, fp := range files {
		for _, p := range neg {
			if fp != p || path.Dir(p) != "." ||
				!scopeFileRe.MatchString(path.Base(p)) {
				continue
			}
			yielded := false
			for _, q := range pos {
				if path.Dir(q) == "." {
					yielded = true
				}
			}
			if !yielded {
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
		// LastSeen is millisecond-truncated — a file written inside
		// the same instant is not "after" the record, so compare
		// against the next millisecond.
		if st, err := os.Stat(p); err == nil && st.ModTime().After(f.LastSeen.Add(time.Millisecond)) {
			return true
		}
	}
	return false
}
