package tools

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/stringext"
	"github.com/charmbracelet/x/ansi"
)

// TruncationProvenance records where content was cut: at capture,
// before the result landed in history, or at recall, on read-back of
// the stored result. The marker text carries it so a reader can tell
// a persistence cap from a retrieval excerpt.
type TruncationProvenance string

const (
	// TruncatedAtCapture marks a write-time cap — a per-tool output
	// limit or the universal tool-result cap.
	TruncatedAtCapture TruncationProvenance = "capture"
	// TruncatedAtRecall marks a read-back bound applied to a stored
	// result being returned through recall.
	TruncatedAtRecall TruncationProvenance = "recall"
)

// TruncateHeadTail caps content at maxBytes, keeping the head and tail
// and replacing the middle with a labeled marker: what was cut, how
// much, where the cut happened, and how to recover. It is byte-based,
// never splits a UTF-8 rune, and never leaves a partial ANSI escape at
// the cut.
func TruncateHeadTail(content string, maxBytes int, prov TruncationProvenance) string {
	if len(content) <= maxBytes {
		return content
	}
	half := maxBytes / 2
	headEnd := stringext.CutANSISafeLeft(content, half)
	tailStart := stringext.CutANSISafeRight(content, len(content)-half)
	head := content[:headEnd]
	tail := content[tailStart:]
	omitted := content[headEnd:tailStart]
	return fmt.Sprintf("%s\n\n... [%d lines (%s) truncated at %s; re-run or re-view the source for the rest] ...\n\n%s",
		head, strings.Count(omitted, "\n"), humanBytes(int64(len(omitted))), prov, tail)
}

// TruncateOutput caps tool output at MaxOutputLength at capture time.
// It is the standard per-tool output bound. The dropped middle is
// written in full to a file under spillDir so the agent can read it
// back rather than losing it for good; an empty spillDir falls back
// to the system temp directory, and a failed spill degrades the
// marker to the re-run hint instead of a path.
func TruncateOutput(content, spillDir string) string {
	if len(content) <= MaxOutputLength {
		return content
	}
	half := MaxOutputLength / 2
	headEnd := stringext.CutANSISafeLeft(content, half)
	tailStart := stringext.CutANSISafeRight(content, len(content)-half)
	head := content[:headEnd]
	tail := content[tailStart:]
	omitted := content[headEnd:tailStart]
	recover := "re-run or re-view the source for the rest"
	if path, err := shell.SpillOutput(ansi.Strip(content), spillDir); err == nil {
		recover = "full output: " + path
	} else {
		slog.Debug("Could not spill tool output", "dir", spillDir, "error", err)
	}
	return fmt.Sprintf("%s\n\n... [%d lines (%s) truncated at %s; %s] ...\n\n%s",
		head, strings.Count(omitted, "\n"), humanBytes(int64(len(omitted))), TruncatedAtCapture, recover, tail)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
