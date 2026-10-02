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
	`java|kt|rb|sh|bash|zsh|fish|md|json|ya?ml|toml|mod|sum|sql|proto|css|scss|html|vue|svelte|mk|cfg|ini)\b`)

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

// scopeResetRe marks cue reset points — a negation ends at a
// conjunction that starts a new directive: "don't touch decoy/, but
// fix main.go" negates decoy only.
var scopeResetRe = regexp.MustCompile(`(?i)\b(?:but|however|instead|then|while|whereas|afterwards?)\b`)

// scopeVerbResetRe marks the other reset class — a directive verb
// restarts intent after a comma: "don't touch decoy/, fix main.go"
// negates decoy only. A verb that is itself the negated predicate
// ("do not fix main.go") or a noun ("the test") is not a reset —
// see directiveVerb.
var scopeVerbResetRe = regexp.MustCompile(`(?i)\b(?:fix|change|update|edit|patch|implement|create|add|modify|handle|correct|repair|address|resolve|rework|rewrite|refactor|adjust|revert|build|rebuild|restore|run|test|check|verify|validate|retry|execute|start|restart|touch|keep|open|watch|use|make)\b`)

// scopeDeterminers make the following word a noun phrase, not a
// directive — "the test" in "don't touch the test in decoy/" is an
// object, so it must not reset the negation and free decoy.
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
// excluded). Polarity is positional: a path is negative iff a negation
// cue precedes it in its clause with no reset conjunction between —
// "fix main.go, do not touch decoy/" keeps main.go positive because
// its own span carries no cue.
func promptScope(prompt, workDir string) (pos, neg []string) {
	for _, c := range splitClauses(prompt) {
		rest := scopeSlashPathRe.ReplaceAllString(c, " ")
		for _, loc := range scopeSlashPathRe.FindAllStringIndex(c, -1) {
			tok := c[loc[0]:loc[1]]
			p := normScopePath(tok)
			if p == "" {
				continue
			}
			negated := cuePrecedes(c, loc[0])
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
			if cuePrecedes(rest, loc[0]) {
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
		for _, loc := range scopeBareWordRe.FindAllStringIndex(rest, -1) {
			w := rest[loc[0]:loc[1]]
			lw := strings.ToLower(w)
			if len(lw) < 3 || scopeStopWords[lw] {
				continue
			}
			if st, err := os.Stat(filepath.Join(workDir, w)); err == nil && st.IsDir() {
				if cuePrecedes(rest, loc[0]) {
					neg = append(neg, w)
				} else {
					pos = append(pos, w)
				}
			}
		}
	}
	return dedupeScope(pos), dedupeScope(neg)
}

// cuePrecedes reports whether the text before offset carries an
// exclusion cue more recent than any reset — a reset conjunction or
// a directive verb that isn't itself negated.
func cuePrecedes(text string, offset int) bool {
	prefix := text[:offset]
	lastNeg, lastReset := 0, 0
	if idx := scopeNegationRe.FindAllStringIndex(prefix, -1); idx != nil {
		lastNeg = idx[len(idx)-1][1]
	}
	if idx := scopeResetRe.FindAllStringIndex(prefix, -1); idx != nil {
		lastReset = idx[len(idx)-1][1]
	}
	for _, v := range scopeVerbResetRe.FindAllStringIndex(prefix, -1) {
		if v[1] > lastReset && directiveVerb(prefix, v[0]) {
			lastReset = v[1]
		}
	}
	return lastNeg > lastReset
}

// directiveVerb reports whether the verb at vstart starts a fresh
// directive — it does not when a negation cue ends immediately before
// it ("do not fix main.go" negates the fix, so the path stays
// negative) or a determiner precedes it ("the fix" is a noun). In
// "don't touch decoy/, fix main.go" the post-comma fix is a fresh
// directive and resets.
func directiveVerb(prefix string, vstart int) bool {
	pre := strings.TrimRight(prefix[:vstart], " \t")
	for _, m := range scopeNegationRe.FindAllStringIndex(pre, -1) {
		if m[1] == len(pre) {
			return false
		}
	}
	if m := lastWordRe.FindStringSubmatch(pre); m != nil &&
		scopeDeterminers[strings.ToLower(m[1])] {
		return false
	}
	return true
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

// cmdTargets extracts the command's repo-relative target args.
// "go test -count=1 ./decoy" → ["decoy"]; "go test ." → ["."]; a Go
// recursive pattern sheds its "/..." tail to the parent dir ("go test
// ./decoy/..." → "decoy"); a command with no path args returns nil —
// its scope is its CWD. Quoted single-word args ("./decoy") unwrap.
// Composite commands mint scope from every segment — "go test . &&
// rm -rf decoy/" binds both "." and "decoy" — because cmdlog keeps no
// per-segment argv; under negation that over-rejects, under explicit
// scope it over-admits. Known distortion, same resolution limit the
// ledger itself has.
func cmdTargets(cmd string) []string {
	var out []string
	skipValue := false
	for _, tok := range strings.Fields(cmd) {
		if skipValue {
			skipValue = false
			continue
		}
		tok = strings.Trim(tok, `"'`)
		if tok == "-args" {
			// Everything after -args belongs to the test binary.
			break
		}
		if strings.HasPrefix(tok, "-") {
			skipValue = cmdValueFlags[tok]
			continue
		}
		if !cmdPathTokenRe.MatchString(tok) {
			continue
		}
		t := strings.TrimPrefix(tok, "./")
		t = strings.TrimSuffix(t, "/...")
		if t == "..." || t == "" || t == "." {
			out = append(out, ".")
		} else {
			out = append(out, path.Clean(t))
		}
	}
	return out
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
//   - Under ambiguity only top-scope verification rows bind — a
//     legitimately-failing subpackage row never surfaces for "the
//     test fails".
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
//     file-form exclusion yields to same-dir positive scope.
//   - An empty prompt (attachment-only turns) binds nothing —
//     referent_none, consistent fail-closed.
func selectOpenFailures(prompt string, failures []cmdlog.Failure, workDir string) ([]cmdlog.Failure, []FailureDecision) {
	if len(failures) == 0 {
		return nil, nil
	}
	if m := interruptedRequestRe.FindStringSubmatch(prompt); m != nil {
		prompt = m[1]
	}
	prompt = selectorPromptReplacer.Replace(prompt)
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
		case negatedByAny(dirs, neg, pos):
			reason = failNegatedScope
		case explicit && !overlapsAny(dirs, pos):
			reason = failOutOfScope
		case explicit && !slices.Contains(verificationKinds, kind):
			// Explicit scope still requires a verdict-flavored row —
			// a failed ls/git/curl at root must not inject into an
			// unrelated "fix main.go".
			reason = failKindMismatch
		case !explicit && kinds == nil:
			reason = failReferentNone
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

func negatedByAny(dirs, neg, pos []string) bool {
	for _, d := range dirs {
		for _, p := range neg {
			if dirWithinNegated(d, p, pos) {
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
