package redact

import (
	"regexp"
	"strings"
)

// redactedPlaceholder replaces a secret-shaped match in outbound
// text. The match is removed whole rather than masked in place so no
// partial credential survives a boundary-case regex.
const redactedPlaceholder = "[REDACTED]"

// secretPatterns are the outbound redaction rules applied to text
// before it leaves the machine via mem0 sync or lands in a durable
// project-memory row. Entries summarize tool output, and that output
// can hold the contents of a .env or a config the agent read — the
// memory server and the shared .crush database are different trust
// domains, so credential-shaped material is stripped at the boundary.
// The list is deliberately pattern-shaped — named keys, auth headers,
// recognizable token formats — rather than entropy-based, so hashes
// and identifiers survive while credential material does not.
// Heuristic, not a guarantee.
var secretPatterns = []*regexp.Regexp{
	// KEY=VALUE / "key": "value" assignments on sensitive names:
	// API_KEY=..., "password": "...", AWS_SECRET_ACCESS_KEY=....
	regexp.MustCompile(`(?i)["'` + "`" + `]?\b[\w.-]*(?:api[_-]?key|secret|token|password|passwd|private[_-]?key|access[_-]?key|credential)[\w.-]*["'` + "`" + `]?\s*[:=]\s*["'` + "`" + `]?[^\s"'` + "`" + `,}]+`),
	// Authorization headers.
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	// Recognizable provider token formats: sk-..., xoxb-..., ghp_...,
	// github_pat_..., glpat-..., ya29...., npm_..., sq0atp-....
	regexp.MustCompile(`\b(?:sk|pk|xox[baprs]|gh[pousr]|glpat|ya29|dop_v1|npm|sq0[a-z]{3})[-_][A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// PEM private key blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`),
	// JWTs: header.payload.signature in base64url. Every real JWT's
	// header starts "eyJ" ({"…), so the anchor keeps false positives
	// rare without an entropy check.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
}

// credentialPrefixPatterns keep their first capture group so the
// scrubbed text still reads as what it was: "postgres://…@host"
// stays a postgres URL instead of an anonymous [REDACTED] gap.
var credentialPrefixPatterns = []*regexp.Regexp{
	// URL-embedded credentials: scheme://user:pass@host — connection
	// strings carry no sensitive key name for the generic rule to
	// catch, but the user:pass authority is always credential-shaped.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`),
}

// Secrets scrubs credential-shaped substrings from text bound for a
// different trust domain. It errs toward named/format-shaped matches:
// a false positive costs one word of an entry, a false negative costs
// a credential leaving the machine or persisting in project memory.
func Secrets(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, redactedPlaceholder)
	}
	for _, p := range credentialPrefixPatterns {
		s = p.ReplaceAllString(s, "${1}"+redactedPlaceholder+"@")
	}
	return s
}

var (
	pemBeginLine = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEndLine   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
)

// SecretsLinePreserving redacts like Secrets but keeps the line
// count: multi-line matches (PEM blocks) are scrubbed line by line,
// so positions computed against the unscrubbed text still hold.
// hunk_after slices windows AFTER redaction — a whole-text scrub
// would let a window cut inside a key block and leak the middle
// lines past the BEGIN-less region (#400).
func SecretsLinePreserving(s string) string {
	lines := strings.Split(s, "\n")
	inPEM := false
	for i, l := range lines {
		switch {
		case inPEM:
			lines[i] = redactedPlaceholder
			if pemEndLine.MatchString(l) {
				inPEM = false
			}
		case pemBeginLine.MatchString(l):
			lines[i] = redactedPlaceholder
			inPEM = true
		default:
			lines[i] = Secrets(l)
		}
	}
	return strings.Join(lines, "\n")
}
