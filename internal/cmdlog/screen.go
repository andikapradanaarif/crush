package cmdlog

import (
	"regexp"
	"strings"
	"unicode"
)

// Injection screening for failure-memory headlines (#219). A headline
// is captured tool output — a test, build tool, or malicious
// dependency prints it — and it replays into later prompts as
// <open_failures> memory, so the write side is the choke point where
// attacker text must shrink before it persists. The screen is
// deliberately phrase-level: it strips override, role-override,
// channel-marker, and exfiltration shapes, not generic imperatives —
// "you should run go mod tidy" is legitimate tool advice, and a
// screen that eats ordinary diagnostics falsifies the memory it
// protects. Scrubbed spans leave a visible [filtered] marker, so the
// rendered row, its signature, and the decisions audit all show that
// screening acted rather than hiding it.

// filteredSpan marks a span the screen removed. Deliberately a plain
// bracketed word: it renders safely (tailSafeText owns angle
// brackets), survives signature hashing, and greps distinct from
// redact's credential marker.
const filteredSpan = "[filtered]"

// filteredHeadlinePlaceholder stands in when nothing benign survives
// screening — the failure event persists, its payload does not.
const filteredHeadlinePlaceholder = "[headline filtered]"

// injectionPatterns are phrase-level shapes a diagnostic line should
// never carry. Precision over recall: each pattern requires
// injection-specific vocabulary so ordinary stderr ("you should run
// go mod tidy", "test run failed") passes untouched, and windows
// stop at sentence boundaries so a trigger word cannot reach across
// a full stop into a benign noun.
var injectionPatterns = []*regexp.Regexp{
	// Context override: "ignore all previous instructions",
	// "disregard the above rules", "override your guardrails".
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override|discard|bypass)\b[^.!?;]{0,45}\b(?:instructions?|prompts?|messages?|rules?|guidelines?|directives?|guardrails?|constraints|programming)\b`),
	// Object-less override: "ignore everything before", "disregard
	// all of the above".
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|discard)\s+(?:everything|anything)\b[^.!?;]{0,30}\b(?:before|above|prior|previous|else)\b`),
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|discard)\s+all\s+of\s+(?:the\s+)?above\b`),
	// Channel/role markers and model special tokens — case-insensitive
	// like the rest; a lowercase [sys] spoofs the same channel.
	regexp.MustCompile(`(?i)<\|[^|]{0,40}\|>|\[/?(?:INST|SYS|SYSTEM)\]|<<\s*/?SYS\s*>>|###\s*(?:instruction|system|override)`),
	regexp.MustCompile(`(?i)\b(?:new|updated|real|actual|true|revised)\s+(?:system\s+)?(?:instructions?|directives?|task|objective|mission|purpose|orders?)\s*:`),
	// Identity override: "you are now", "pretend to be" — plus
	// "act as"/"behave as" only when a persona noun follows, so
	// ordinary prose ("env vars act as overrides") survives.
	regexp.MustCompile(`(?i)\b(?:you\s+are\s+now|you're\s+now|pretend\s+(?:to\s+be|you\s+are|you're)|imagine\s+you\s+are|from\s+now\s+on\s+you\s+(?:are|will|must))\b`),
	regexp.MustCompile(`(?i)\b(?:act|behave)\s+as\s+(?:an?\s+|the\s+)?(?:\w+\s+){0,2}(?:root|admin|administrator|superuser|assistant|chatbot|agent|ai)\b`),
	// Exfiltration and response suppression: "reveal your system
	// prompt", "print your api keys", "do not respond".
	regexp.MustCompile(`(?i)\b(?:reveal|show|print|output|repeat|leak|expose|display|dump|echo|tell\s+me)\s+(?:me\s+|us\s+|the\s+user\s+)?(?:your|the\s+full|the\s+entire|all\s+your|any)\s+(?:system\s+prompt|instructions?|rules?|secrets?|api[\s_-]?keys?|tokens?|passwords?|credentials?)\b`),
	regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never)\s+(?:respond|answer|reply|mention|tell\s+the\s+user|reveal|disclose|alert|warn)\b`),
}

// ansiEscRe matches terminal escape sequences a headline can smuggle:
// CSI (colors, cursor moves), OSC (title setting, hyperlinks —
// terminated by BEL or ST), and the remaining two-byte ESC forms. A
// headline renders into the TUI later, so escape bytes must die at
// the boundary, not at render time.
var ansiEscRe = regexp.MustCompile("\x1b(?:\\[[0-9;:?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\\([0-9A-Za-z]|[@-Z\\\\-~])")

// ScreenHeadline removes what a persisted failure line must not
// carry into later prompts: terminal escape sequences, non-printing
// and format runes (zero-width, bidi overrides, private use), and
// instruction-shaped spans. Each scrubbed span leaves [filtered] so
// the stored row advertises that it was cut; a line reduced to
// markers and punctuation alone returns the placeholder — the
// failure record is worth keeping, its text is not. It runs at
// persist and again at render: rows written before the screen
// existed, or by a future unscreened path, still get screened on
// the way into the prompt.
func ScreenHeadline(headline string) string {
	s := ansiEscRe.ReplaceAllString(headline, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || unicode.Is(unicode.Zs, r):
			return ' '
		case unicode.IsPrint(r):
			return r
		default:
			return -1
		}
	}, s)
	for _, re := range injectionPatterns {
		s = re.ReplaceAllString(s, filteredSpan)
	}
	s = strings.Join(strings.Fields(s), " ")
	// Markers plus punctuation is still all-payload — "[filtered]."
	// carries no benign text.
	if s != "" && !strings.ContainsFunc(strings.ReplaceAll(s, filteredSpan, ""),
		func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return filteredHeadlinePlaceholder
	}
	return s
}
