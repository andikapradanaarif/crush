// Package toolclass holds the shared tool-call classification
// vocabulary — which calls read, write, or emit re-derivable output —
// in a leaf package so both the agent layer (scope gate, stubs,
// verifying tools) and the notebook layer (checkpoint boundary
// detection) can depend on it without an import cycle.
package toolclass

import (
	"encoding/json"
	"regexp"
	"strings"
)

// DownloadToolName mutates files when its result lands — a write for
// boundary purposes even though the write is a side effect.
const DownloadToolName = "download"

// WriteToolNames mutate files; a successful result supersedes earlier
// reads of the same path. The set is also the verifyingTool wrap set and
// the gate's metadata-scan set — lsp_rename/lsp_replace_symbol mutate
// via workspace edits, the canonical caller-breaker.
var WriteToolNames = map[string]bool{
	"edit": true, "write": true, "multiedit": true,
	"lsp_rename": true, "lsp_replace_symbol": true,
}

// ReadToolNames capture file content; their results go stale on writes.
var ReadToolNames = map[string]bool{"view": true, "read": true}

// CommandToolNames emit re-derivable output: once a result is old
// enough to leave the recency guard, or a re-run makes it redundant,
// a labeled stub suffices.
var CommandToolNames = map[string]bool{"bash": true, "grep": true, "glob": true, "ls": true}

// mutatingBashRe matches shell commands that mutate files or git state
// — the "large, destructive, hard to reverse" calls that must not
// bypass the write boundary just because they arrive through bash
// instead of a write tool. Deliberately conservative in both
// directions: mutations hidden inside scripts or build targets (make,
// go generate) pass un-gated, and read-ish commands that merely touch
// state (git config --get) stay exploration. A false positive costs
// one confirmation question; a false negative skips the checkpoint.
//
// The scan runs on the raw command text — command names inside quoted
// spans still match ("bash -c 'rm -rf /'" gates) — while the redirect
// check below masks quoted spans so "echo 'a > b'" stays exploration.
var mutatingBashRe = regexp.MustCompile(`\b(rm|rmdir|mv|cp|dd|truncate|shred|chmod|chown|chgrp|ln|tee|patch|install|touch|mkdir|rsync|scp)\b|` +
	`\b(sed|perl)\s+(-\S+\s+)*(-\S*i|-i\S*|--in-place)\b|` +
	`\bgit\s+(commit|push|reset|checkout|switch|restore|clean|rebase|merge|am|apply|stash|tag|revert|cherry-pick|mv|rm|init|clone|pull|bisect|submodule|update-ref|notes|branch\s+-[dDmM])\b|` +
	`\bapt(-get)?\s+(install|remove|purge|upgrade|update|dist-upgrade)\b|` +
	`\bkubectl\s+(delete|apply|create|patch|edit|replace|scale|drain|cordon|uncordon)\b`)

// redirectTargetRe finds shell redirects and their targets; writing to
// a real file mutates it, while fd duplication and /dev/null do not.
var redirectTargetRe = regexp.MustCompile(`>>?\s*(\S+)`)

// fdDupTargetRe matches the fd-duplication redirect targets that are
// not file writes — `>&1`, `>&-` — as opposed to `>&out`, which is
// bash's stdout+stderr-to-file form and does mutate.
var fdDupTargetRe = regexp.MustCompile(`^&[-\d]`)

// quotedSpanRe masks single- and double-quoted spans before the
// redirect scan: a `>` inside a string literal must not gate, while a
// quoted *target* (`> 'out'`) still counts — masking to a placeholder
// keeps the target position occupied.
var quotedSpanRe = regexp.MustCompile(`'[^']*'|\$'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)

// testExprRe masks [[ ]] conditional expressions — a `>` inside is a
// string comparison, not a redirect.
var testExprRe = regexp.MustCompile(`\[\[[^\]]*\]\]`)

// arithRe masks (( )) and $(( )) arithmetic — a `>` inside is a
// numeric comparison.
var arithRe = regexp.MustCompile(`\$?\(\([^)]*\)\)`)

// heredocRe finds heredoc openers and captures the delimiter from
// the raw command (quoted delimiters arrive masked on the scan
// string, so capture happens on the original text).
var heredocRe = regexp.MustCompile(`<<(-?)[ \t]*(?:'([A-Za-z0-9_]+)'|"([A-Za-z0-9_]+)"|([A-Za-z0-9_]+))`)

// maskCommand returns the command with every span a `>` can hide
// inside blanked to spaces — quoted literals, [[ ]] tests, (( ))
// arithmetic, and heredoc bodies. The result is position-preserving:
// byte offsets in the masked string index the original command, so
// operators found there extract their targets from the raw text.
// Both the mutation classifier and the redirect-target extractor
// scan this form so they never disagree on what a `>` means.
func maskCommand(command string) string {
	masked := quotedSpanRe.ReplaceAllStringFunc(command, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
	masked = testExprRe.ReplaceAllStringFunc(masked, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
	masked = arithRe.ReplaceAllStringFunc(masked, func(s string) string {
		return strings.Repeat(" ", len(s))
	})
	return maskHeredocBodies(command, masked)
}

// commentStripped blanks unquoted comments — `#` to end of line —
// position-preserving like maskCommand. A `#` that survives
// maskCommand is unquoted by construction; bash comments start only
// at word start (position 0, after whitespace, or after a
// metachar), so mid-word forms (`a#b`, `${x#y}`, `$#`) and
// quote-glued forms (`"x"#y`) survive. Without this a separator
// inside a comment (`go build # TODO: sed -i …; tee log`) splits a
// phantom segment and comments feed the operand scans — both
// manufacture targets for writes that never happen.
func commentStripped(command string) string {
	masked := maskCommand(command)
	out := []byte(command)
	for i := 0; i < len(masked); i++ {
		if masked[i] != '#' {
			continue
		}
		if i > 0 {
			// Boundary check on the original: a blanked quote span
			// looks like whitespace in masked, but `"x"#y` glues
			// mid-word all the same.
			prev := command[i-1]
			if !strings.ContainsRune(" \t\n;|&()<>", rune(prev)) {
				continue
			}
		}
		for j := i; j < len(masked) && masked[j] != '\n'; j++ {
			out[j] = ' '
		}
	}
	return string(out)
}

// maskHeredocBodies blanks each heredoc body — from the line after
// the << token to its delimiter line — so a `>` inside body text is
// not a redirect. An unterminated heredoc masks to end of command,
// matching bash's own treatment of the remaining text as body.
func maskHeredocBodies(command, masked string) string {
	b := []byte(masked)
	for _, m := range heredocRe.FindAllStringSubmatchIndex(command, -1) {
		// A `<<` preceded by another `<` is a herestring (`<<<`),
		// and an opener inside a quoted span is literal text —
		// neither starts a heredoc.
		if (m[0] > 0 && command[m[0]-1] == '<') || masked[m[0]] == ' ' {
			continue
		}
		stripTabs := m[2] >= 0 && command[m[2]:m[3]] == "-"
		var delim string
		for i := 4; i <= 8; i += 2 {
			if m[i] >= 0 {
				delim = command[m[i]:m[i+1]]
				break
			}
		}
		if delim == "" {
			continue
		}
		nl := strings.IndexByte(command[m[1]:], '\n')
		if nl < 0 {
			continue
		}
		bodyStart := m[1] + nl + 1
		pos := bodyStart
		for pos <= len(command) {
			lineEnd := strings.IndexByte(command[pos:], '\n')
			line := command[pos:]
			if lineEnd >= 0 {
				line = command[pos : pos+lineEnd]
			}
			candidate := line
			if stripTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == delim {
				break
			}
			if lineEnd < 0 {
				pos = len(command) + 1
				break
			}
			pos += lineEnd + 1
		}
		for i := bodyStart; i < pos && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// IsMutatingCall classifies a call as a write for boundary purposes:
// a write-tool name, a file-writing download, or a bash command whose
// text matches a mutating pattern or a file-writing redirect. It is
// the single mutation vocabulary shared by the scope gate and the
// notebook checkpoint's write-boundary detector — the two boundaries
// must agree on what counts as a write.
func IsMutatingCall(name, input string) bool {
	if WriteToolNames[name] || name == DownloadToolName {
		return true
	}
	if name != "bash" {
		return false
	}
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil || params.Command == "" {
		return false
	}
	if mutatingBashRe.MatchString(params.Command) {
		return true
	}
	// The redirect check reuses the extractor in its lenient form —
	// any redirect target counts as a write (`> $OUT`, `> ~/out`,
	// `> *.log` all mutate even though the path can't be bound),
	// while the evidence extractor stays strict about concreteness.
	return len(bashRedirectTargets(params.Command, false)) > 0
}

// redirectOpRe finds redirect operators outside quoted spans (the
// masked string keeps `>` inside literals from matching).
var redirectOpRe = regexp.MustCompile(`>>?`)

// BashRedirectTargets extracts the file paths a bash command writes
// via redirect — `> out`, `>> out`, `>& out` — for evidence scans that
// need the mutated path, not just the mutation verdict. Operators are
// found on the quote-masked command so a `>` inside a string literal
// produces no target; targets are then parsed from the ORIGINAL text,
// so a quoted target (`> 'out'`) resolves to its real path — matching
// what IsMutatingCall classifies as a write. Non-redirect mutations
// (sed -i, tee, cp) have no extractable target here; they classify as
// mutating via IsMutatingCall but yield no path.
func BashRedirectTargets(command string) []string {
	return bashRedirectTargets(command, true)
}

// bashRedirectTargets is the shared scan behind BashRedirectTargets
// and IsMutatingCall's redirect check. With concreteOnly, targets
// needing shell expansion are dropped — evidence must name the path
// that was actually written. Without it every real target counts —
// `> $OUT` is still a mutation even when its path can't be bound.
func bashRedirectTargets(command string, concreteOnly bool) []string {
	// `>` inside a comment is literal text (`cmd #> f` writes
	// nothing); strip before the mask so the operator scan never
	// sees it.
	command = commentStripped(command)
	masked := maskCommand(command)
	var out []string
	for _, loc := range redirectOpRe.FindAllStringIndex(masked, -1) {
		i := loc[1]
		// `>&word` writes both streams to a file — a `&` glued to the
		// `>` is part of the combined-stream operator, not the path.
		// After whitespace (`> &file`) it's a literal filename char.
		glued := i < len(command) && command[i] == '&'
		if glued {
			i++
		}
		for i < len(command) && (command[i] == ' ' || command[i] == '\t') {
			i++
		}
		if i >= len(command) {
			continue
		}
		var target string
		if q := command[i]; q == '\'' || q == '"' {
			end := strings.IndexByte(command[i+1:], q)
			if end < 0 {
				continue
			}
			target = command[i+1 : i+1+end]
		} else {
			// Unquoted targets end at the first unescaped separator;
			// backslash escapes stay in the path (`my\ file` →
			// `my file`), matching how the shell word-splits.
			j := i
			var sb strings.Builder
			for j < len(command) && !strings.ContainsRune(" \t\n;|<>()", rune(command[j])) {
				if command[j] == '\\' && j+1 < len(command) {
					sb.WriteByte(command[j+1])
					j += 2
					continue
				}
				sb.WriteByte(command[j])
				j++
			}
			target = sb.String()
		}
		if target == "" || target == "/dev/null" {
			continue
		}
		// fd duplication (`>&1`, `>&-`) is not a file write.
		if glued && fdDupTargetRe.MatchString("&"+target) {
			continue
		}
		if !concreteOnly || concreteRedirectTarget(target) {
			out = append(out, target)
		}
	}
	return out
}

// concreteRedirectTarget reports whether a redirect target resolves
// to a definite path without expansion. Targets needing shell
// expansion — `~/out`, `$OUT`, `*.log`, `$(gen)` — yield an
// uncertain path at evidence time, so they're dropped rather than
// recorded as writes that never verifiably happened.
func concreteRedirectTarget(target string) bool {
	return !strings.HasPrefix(target, "~") &&
		!strings.ContainsAny(target, "$`*?[{")
}

// BashEditTargets extracts the file paths a bash command writes file
// CONTENT to through the routes an agent would take instead of a
// write tool — redirects (`> out`, `>> out`, `>& out`), in-place
// stream edits (`sed -i f`, `perl -pi f`), and `tee f`. Every
// returned path is concrete: targets that need shell expansion are
// dropped, since an evidence check can only bind a definite path.
// Mutations that move or destroy rather than author content — cp,
// mv, rm, chmod, git plumbing — carry no read-evidence expectation
// and yield no targets. The scan is deliberately conservative: a
// writer it cannot see (patch diffs, make targets, `bash -c`
// scripts, `xargs`/`find -exec` operand feeds, variable file
// lists) passes un-gated rather than guess.
func BashEditTargets(command string) []string {
	out := bashRedirectTargets(command, true)
	seen := make(map[string]bool, len(out))
	for _, t := range out {
		seen[t] = true
	}
	for _, seg := range maskedSegments(command) {
		args := shellWords(seg)
		if len(args) == 0 {
			continue
		}
		// Pipeline leftovers and leading modifiers sit in argv[0]'s
		// slot: `cmd |& tee f` opens a segment on `&`, and `sudo`,
		// `time`, `env A=1`, … prefix the real writer. Skip them —
		// flag words after a prefix stay un-gated rather than guess
		// each flag's arity.
		for len(args) > 1 {
			switch baseCommand(args[0]) {
			case "&", "!", "sudo", "doas", "time", "nice", "nohup",
				"command", "builtin", "exec":
				args = args[1:]
				continue
			case "env":
				args = args[1:]
				for len(args) > 0 && isEnvAssign(args[0]) {
					args = args[1:]
				}
				continue
			}
			break
		}
		var files []string
		switch baseCommand(args[0]) {
		case "sed", "perl":
			files = inPlaceTargets(args)
		case "tee":
			files = teeTargets(args)
		}
		for _, f := range files {
			if !seen[f] && concreteRedirectTarget(f) {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// baseCommand strips directory components so `/usr/bin/sed` and
// `gsed`-style paths classify by their binary name.
func baseCommand(arg string) string {
	return arg[strings.LastIndex(arg, "/")+1:]
}

// isEnvAssign reports whether a word is a NAME=value environment
// assignment — the word form `env` consumes before its command.
func isEnvAssign(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := word[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// maskedSegments splits a command into simple-command segments on
// unquoted ; | & and newlines. Positions come from the quote-masked
// form so separators inside literals (`echo 'a; b'`) never split,
// and comments strip first so a `#` inside one manufactures no
// segment or operands; the returned segments slice the
// comment-stripped text. Command substitutions like
// `$(sed -i 's/x/y/' f)` survive unmasked, so writers nested in
// substitutions still surface as segments.
func maskedSegments(command string) []string {
	stripped := commentStripped(command)
	masked := maskCommand(stripped)
	var segs []string
	start := 0
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '|', '&', ';', '\n':
			if seg := strings.TrimSpace(stripped[start:i]); seg != "" {
				segs = append(segs, seg)
			}
			start = i + 1
		}
	}
	if seg := strings.TrimSpace(stripped[start:]); seg != "" {
		segs = append(segs, seg)
	}
	return segs
}

// shellWords splits a segment into shell words: whitespace outside
// quotes separates, quote marks strip, backslash escapes the next
// byte. An empty quoted span yields an empty word — BSD `sed -i ”`
// needs the empty suffix visible as an operand slot.
func shellWords(seg string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' && i+1 < len(seg) {
				cur.WriteByte(seg[i+1])
				i++
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\' && i+1 < len(seg):
			cur.WriteByte(seg[i+1])
			inWord = true
			i++
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// redirectWordSkip reports how many argv slots a redirect token
// occupies — 2 for a bare operator (`2>` + its separate target, or
// `>&` + `out`), 1 for a glued op+target (`>out`, `2>&1`) — so
// operand scans skip both forms and never misread a redirect target
// as a file operand. The targets themselves are already covered by
// the redirect scan.
func redirectWordSkip(args []string, i int) int {
	tok := args[i]
	if !strings.ContainsAny(tok, "<>") {
		return 0
	}
	pure := true
	for j := 0; j < len(tok); j++ {
		if !strings.ContainsRune("0123456789<>|&", rune(tok[j])) {
			pure = false
			break
		}
	}
	if !pure {
		return 1 // Glued op+target (`>out`, `2>f`, `<<EOF`).
	}
	// A pure operator token ending on an operator char takes its
	// target as the next word; one ending on a digit is a complete
	// fd duplication (`2>&1`).
	switch tok[len(tok)-1] {
	case '>', '<', '&', '|':
		return 2
	}
	return 1
}

// inPlaceTargets returns the file operands of an in-place stream
// edit — `sed -i 's/a/b/' f`, `perl -pi -e 's/a/b/' f` — or nil when
// the call edits nothing in place. The first non-flag operand is the
// script unless -e/-f/--expression/--file named it explicitly;
// every operand after the script is a file. A standalone -i followed
// by a word that is not a flag and not script-shaped is the BSD
// backup-suffix form (`sed -i .bak`, `sed -i ”`), so the suffix is
// consumed rather than treated as the script.
func inPlaceTargets(args []string) []string {
	prog := baseCommand(args[0])
	inPlace, scriptViaFlag, afterDD := false, false, false
	var operands []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case afterDD:
			operands = append(operands, a)
		case a == "--":
			afterDD = true
		case a == "":
			// An empty word can only be a BSD -i suffix slot, which
			// the -i case consumes — never a file.
		case redirectWordSkip(args, i) > 0:
			i += redirectWordSkip(args, i) - 1
		case strings.HasPrefix(a, "--"):
			switch {
			case a == "--in-place" || strings.HasPrefix(a, "--in-place="):
				inPlace = true
			case a == "--expression" || a == "--file":
				scriptViaFlag = true
				i++
			case strings.HasPrefix(a, "--expression="), strings.HasPrefix(a, "--file="):
				scriptViaFlag = true
			}
			// Other long flags (--quiet, --posix, …) consume no value.
		case len(a) > 1 && a[0] == '-':
			b := a[1:]
			switch {
			case b == "e" || b == "f" && prog == "sed":
				// `-e script` / `-f scriptfile` take a separate value.
				scriptViaFlag = true
				i++
			case prog == "sed" && (strings.HasPrefix(b, "e") || strings.HasPrefix(b, "f")),
				prog == "perl" && strings.HasPrefix(b, "e"):
				// Attached script values: `-e's/a/b/'`, `-fFILE`.
				scriptViaFlag = true
			case strings.Contains(b, "i"):
				// A lowercase i in any short-flag bundle marks the
				// in-place form: -i, -ni, -pi.bak. Perl's -I include
				// path is uppercase and cannot collide.
				inPlace = true
				if b == "i" && i+1 < len(args) &&
					(args[i+1] == "" || (args[i+1][0] != '-' && !looksLikeScript(args[i+1]))) {
					i++ // BSD `-i SUFFIX`: consume the backup suffix.
				}
			}
			// Other bundles (-n, -E, -p) consume no value.
		default:
			operands = append(operands, a)
		}
	}
	if !inPlace {
		return nil
	}
	if !scriptViaFlag && len(operands) > 0 {
		operands = operands[1:] // The leading operand is the script.
	}
	return operands
}

// looksLikeScript reports whether a word plausibly carries the edit
// program rather than a backup suffix — it resolves the BSD -i
// ambiguity: `sed -i .bak 's/x/y/' f` consumes .bak as the suffix,
// while `sed -i 's/x/y/' f` treats the next word as the script.
// Script commands open with one of sed's command letters or carry
// the s/x/y/ and a=b shapes.
func looksLikeScript(word string) bool {
	return word != "" && (strings.ContainsAny(word, "/=") ||
		strings.ContainsRune("sydaicqwrtpbgnGDHlxz{}:!#", rune(word[0])))
}

// teeTargets returns the file operands of a tee call — every non-flag
// argument writes a copy of stdin. tee has no value-taking flags in
// common use (-a, -i, -p and the long forms are booleans;
// --output-error's mode attaches with =).
func teeTargets(args []string) []string {
	var operands []string
	afterDD := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case afterDD:
			operands = append(operands, a)
		case a == "--":
			afterDD = true
		case redirectWordSkip(args, i) > 0:
			i += redirectWordSkip(args, i) - 1
		case strings.HasPrefix(a, "-") && a != "-":
			// Flag — tee's short and long flags take no value.
		default:
			operands = append(operands, a)
		}
	}
	return operands
}
