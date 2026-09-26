package notebook

import "regexp"

// redactedPlaceholder replaces a secret-shaped match in outbound
// text. The match is removed whole rather than masked in place so no
// partial credential survives a boundary-case regex.
const redactedPlaceholder = "[REDACTED]"

// secretPatterns are the outbound redaction rules applied to notebook
// entries before they leave the machine via mem0 sync. Entries
// summarize tool output, and that output can hold the contents of a
// .env or a config the agent read — the memory server is a different
// trust domain, so credential-shaped material is stripped at the
// boundary. The list is deliberately pattern-shaped — named keys,
// auth headers, recognizable token formats — rather than
// entropy-based, so hashes and identifiers survive while credential
// material does not. Heuristic, not a guarantee.
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
}

// redactSecrets scrubs credential-shaped substrings from text bound
// for the memory server. It errs toward named/format-shaped matches:
// a false positive costs one word of an entry, a false negative costs
// a credential on a third-party server.
func redactSecrets(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, redactedPlaceholder)
	}
	return s
}
