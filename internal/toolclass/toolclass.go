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
var heredocRe = regexp.MustCompile(`<<(-?)\s*(?:'([A-Za-z0-9_]+)'|"([A-Za-z0-9_]+)"|([A-Za-z0-9_]+))`)

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

// maskHeredocBodies blanks each heredoc body — from the line after
// the << token to its delimiter line — so a `>` inside body text is
// not a redirect. An unterminated heredoc masks to end of command,
// matching bash's own treatment of the remaining text as body.
func maskHeredocBodies(command, masked string) string {
	b := []byte(masked)
	for _, m := range heredocRe.FindAllStringSubmatchIndex(command, -1) {
		// An opener inside a quoted span is literal text, not a
		// heredoc — the masked string is already blanked there.
		if masked[m[0]] == ' ' {
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
