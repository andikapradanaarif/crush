package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// postEditContextLines is how much surrounding file content each
// changed hunk carries in the response — enough for the model to
// compose the next old_string without a view call when the next
// write lands near this one.
const postEditContextLines = 10

// postEditMaxLines caps the region payload across all hunks: past the
// cap the response notes truncation rather than approaching the size
// of a full view call it exists to replace.
const postEditMaxLines = 50

// hunkHeader matches a unified-diff hunk header, capturing the
// new-file start line and (optional) length — the post-edit span.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// hunkSpan is a changed range in the post-edit file, 1-based. A
// length of 0 marks a pure deletion — start is the junction line.
type hunkSpan struct{ start, length int }

// hunkNewSpans extracts each hunk's post-edit span from a unified
// diff, in file order.
func hunkNewSpans(unified string) []hunkSpan {
	var spans []hunkSpan
	for line := range strings.Lines(unified) {
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		length := 1
		if m[2] != "" {
			length, _ = strconv.Atoi(m[2])
		}
		spans = append(spans, hunkSpan{start, length})
	}
	return spans
}

// withPostEditRegion appends the post-edit region to msg when one can
// be rendered from the diff — the working state the model would
// otherwise fetch with a separate view call.
func withPostEditRegion(msg, unified, newContent string) string {
	if region := postEditRegion(unified, newContent); region != "" {
		return msg + "\n\n" + region
	}
	return msg
}

// postEditRegion renders the regions of newContent (the bytes just
// written — post line-ending normalization) around each changed hunk,
// numbered the way view renders lines. It returns "" when the diff
// carries no usable hunk, so callers can degrade to the bare
// confirmation rather than emit a misleading region.
func postEditRegion(unified, newContent string) string {
	spans := hunkNewSpans(unified)
	if len(spans) == 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(newContent, "\r\n", "\n"), "\n")
	total := len(lines)

	// Each span's display window is the changed span plus context on
	// both sides; a zero-length span is a deletion junction and takes
	// symmetric context around it.
	type window struct{ lo, hi int }
	windows := make([]window, 0, len(spans))
	for _, s := range spans {
		var lo, hi int
		if s.length == 0 {
			lo, hi = s.start-postEditContextLines, s.start+postEditContextLines
		} else {
			lo = s.start - postEditContextLines
			hi = s.start + s.length - 1 + postEditContextLines
		}
		if lo < 1 {
			lo = 1
		}
		if hi > total {
			hi = total
		}
		if lo > hi {
			continue
		}
		// Merge with the previous window when they touch — adjacent
		// hunks share one numbered block.
		if n := len(windows); n > 0 && lo <= windows[n-1].hi+1 {
			windows[n-1].hi = hi
			continue
		}
		windows = append(windows, window{lo, hi})
	}
	if len(windows) == 0 {
		return ""
	}

	var sb strings.Builder
	emitted := 0
	truncated := false
	for wi, w := range windows {
		remaining := postEditMaxLines - emitted
		if remaining <= 0 {
			truncated = true
			break
		}
		// Header must name the range actually emitted, not the
		// window's full extent — the cap can cut a window short.
		showHi := min(w.hi, w.lo+remaining-1)
		if wi > 0 {
			sb.WriteString("     ⋯\n")
		}
		fmt.Fprintf(&sb, "Current file content (lines %d-%d):\n", w.lo, showHi)
		for ln := w.lo; ln <= showHi; ln++ {
			line := lines[ln-1]
			if len(line) > MaxLineLength {
				// Truncate at a rune boundary, as view does.
				line = strings.ToValidUTF8(line[:MaxLineLength], "") + "..."
			}
			fmt.Fprintf(&sb, "%6d|%s\n", ln, line)
			emitted++
		}
		if showHi < w.hi {
			truncated = true
			break
		}
	}
	out := strings.TrimRight(sb.String(), "\n")
	if truncated {
		out += "\n     ⋯ (truncated — run view for more)"
	}
	return out
}
