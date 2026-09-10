package prompt

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// CacheClass describes how stable a prompt section is across requests.
// It is used for cache-breakpoint planning: stable sections can share a
// prefix cache while volatile sections change every session or turn.
type CacheClass string

const (
	// CacheClassStable sections are identical across sessions and turns
	// (core policy, project context files, skills).
	CacheClassStable CacheClass = "stable"
	// CacheClassSession sections are stable within a session but differ
	// across sessions (MCP instructions).
	CacheClassSession CacheClass = "session"
	// CacheClassVolatile sections change frequently (date, git status,
	// notebook context).
	CacheClassVolatile CacheClass = "volatile"
)

// PromptSection is a named, measured component of a built prompt.
type PromptSection struct {
	Name       string
	Content    string
	Required   bool
	Bytes      int
	EstTokens  int64 // populated by approxTokenCount, same heuristic used elsewhere
	CacheClass CacheClass
}

// BuiltPrompt is the rendered system prompt plus per-section telemetry.
type BuiltPrompt struct {
	Text     string
	Sections []PromptSection
}

// taggedSection maps a rendered <tag>...</tag> region to its section
// name and cache class.
type taggedSection struct {
	name       string
	required   bool
	cacheClass CacheClass
}

// sectionTags lists the XML tags emitted by the prompt templates that
// are measured as standalone sections. Everything outside these tags is
// reported as core_policy.
var sectionTags = map[string]taggedSection{
	"env":              {"env", true, CacheClassVolatile},
	"project_context":  {"project_context", true, CacheClassStable},
	"user_preferences": {"user_context", true, CacheClassStable},
	"available_skills": {"skills", true, CacheClassStable},
}

// extractSections scans the rendered prompt for the known tagged
// regions and returns one PromptSection per region plus a core_policy
// section covering the remainder. core_policy is listed first and the
// rest follow in render order.
func extractSections(text string) []PromptSection {
	type found struct {
		section PromptSection
		start   int
	}
	var foundSections []found
	coreBytes := len(text)
	for tag, meta := range sectionTags {
		content, start, ok := extractTag(text, tag)
		if !ok {
			continue
		}
		coreBytes -= len(content)
		foundSections = append(foundSections, found{
			start: start,
			section: PromptSection{
				Name:       meta.name,
				Content:    content,
				Required:   meta.required,
				Bytes:      len(content),
				EstTokens:  approxTokenCount(content),
				CacheClass: meta.cacheClass,
			},
		})
	}
	sort.Slice(foundSections, func(i, j int) bool {
		return foundSections[i].start < foundSections[j].start
	})
	sections := []PromptSection{{
		Name:       "core_policy",
		Required:   true,
		Bytes:      coreBytes,
		EstTokens:  int64((coreBytes + 3) / 4),
		CacheClass: CacheClassStable,
	}}
	for _, f := range foundSections {
		sections = append(sections, f.section)
	}
	return sections
}

// extractTag returns the "<tag>...</tag>" region of text including the
// tags themselves, along with its start offset. The bool reports whether
// both markers were found in order.
func extractTag(text, tag string) (string, int, bool) {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return "", 0, false
	}
	end := strings.Index(text[start:], closeTag)
	if end < 0 {
		return "", 0, false
	}
	return text[start : start+end+len(closeTag)], start, true
}

// approxTokenCount estimates a token count using the same ~4 bytes per
// token heuristic used by the agent's usage fallback.
func approxTokenCount(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}

// tokenLimitToBytes converts a token limit to a conservative byte limit
// using the same ~4 bytes per token heuristic.
func tokenLimitToBytes(tokenLimit int) int {
	return tokenLimit * 4
}

// truncateUTF8Prefix normalizes invalid UTF-8 and trims so that the
// result is valid UTF-8 no longer than maxBytes.
func truncateUTF8Prefix(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "")
	if maxBytes >= len(s) {
		return s
	}
	for end := maxBytes; end > 0; end-- {
		if end == len(s) || utf8.RuneStart(s[end]) {
			return s[:end]
		}
	}
	return ""
}

// truncateToTokenLimit truncates s so that approxTokenCount(result) <=
// tokenLimit. It binary-searches the byte limit, which keeps large
// dense-token inputs linear instead of quadratic.
func truncateToTokenLimit(s string, tokenLimit int) string {
	s = strings.ToValidUTF8(s, "")
	if approxTokenCount(s) <= int64(tokenLimit) {
		return s
	}
	lo, hi := 0, tokenLimitToBytes(tokenLimit)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if approxTokenCount(truncateUTF8Prefix(s, mid)) <= int64(tokenLimit) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return truncateUTF8Prefix(s, lo)
}

// maxContextFileReadSize is the hard process-protection limit for a
// single context file. Files larger than this are truncated; the limit
// is measured in bytes and guards against unbounded context files
// exhausting memory or producing oversized requests.
const maxContextFileReadSize = 256 * 1024

// readBounded reads up to maxContextFileReadSize bytes from path. If
// the file is larger than the hard limit, it returns the truncated
// prefix and truncated=true. The result is always valid UTF-8.
func readBounded(path string) (content string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, maxContextFileReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return truncateUTF8Prefix(string(buf[:n]), n), false, nil
	case err != nil:
		return "", false, err
	default:
		// Read succeeded for max+1 bytes — file is larger than the hard limit.
		return truncateUTF8Prefix(string(buf[:maxContextFileReadSize]), maxContextFileReadSize), true, nil
	}
}

// readContextFile reads a context file enforcing both the hard byte
// limit (via readBounded) and a soft token budget. required controls
// overflow behavior:
//   - required, over soft budget: error. Interactive prompting for this
//     case requires plumbing that does not exist yet (the loader runs
//     far below any TUI question machinery), so the interactive flag
//     currently only documents intent.
//   - optional, over soft budget: truncate via truncateToTokenLimit.
func readContextFile(path string, softTokenLimit int, required, _ bool) (content string, truncated bool, err error) {
	raw, hardTruncated, err := readBounded(path)
	if err != nil {
		return "", false, err
	}
	if approxTokenCount(raw) <= int64(softTokenLimit) {
		return raw, hardTruncated, nil
	}
	if required {
		return "", false, fmt.Errorf(
			"required context file %s exceeds soft token budget (%d tokens); "+
				"reduce the file or raise the budget", path, softTokenLimit,
		)
	}
	// Optional file over budget: truncate.
	return truncateToTokenLimit(raw, softTokenLimit), true, nil
}
