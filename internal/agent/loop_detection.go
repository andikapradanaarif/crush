package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"charm.land/fantasy"
)

const (
	loopDetectionWindowSize = 10
	loopDetectionMaxRepeats = 5
)

// hasRepeatedToolCalls checks whether the agent is stuck in a loop by looking
// at recent steps. It examines the last windowSize steps and returns true if
// any tool-call signature appears more than maxRepeats times.
func hasRepeatedToolCalls(steps []fantasy.StepResult, windowSize, maxRepeats int) bool {
	sig, _, _ := repeatedToolSignature(steps, windowSize, maxRepeats)
	return sig != ""
}

// repeatedToolSignature returns the first tool-interaction signature in
// the detection window whose count exceeds maxRepeats — the hex hash,
// the window step's first tool-call name, and the repeat count. The
// stop signal only needs the bool, but the stall edge's trigger detail
// needs the exact repeated interaction, not the window's dominant tool
// name.
func repeatedToolSignature(steps []fantasy.StepResult, windowSize, maxRepeats int) (sig, toolName string, repeats int) {
	if len(steps) < windowSize {
		return "", "", 0
	}

	window := steps[len(steps)-windowSize:]
	counts := make(map[string]int)
	tools := make(map[string]string)

	for _, step := range window {
		s := getToolInteractionSignature(step.Content)
		if s == "" {
			continue
		}
		if _, ok := tools[s]; !ok {
			tools[s] = firstToolCallName(step.Content)
		}
		counts[s]++
		if counts[s] > maxRepeats {
			return s, tools[s], counts[s]
		}
	}

	return "", "", 0
}

// firstToolCallName returns the name of the first tool call in a step's
// content — the human-readable label for the signature that step
// produced.
func firstToolCallName(content fantasy.ResponseContent) string {
	for _, tc := range content.ToolCalls() {
		return tc.ToolName
	}
	return ""
}

// getToolInteractionSignature computes a hash signature for the tool
// interactions in a single step's content. It pairs tool calls with their
// results (matched by ToolCallID) and returns a hex-encoded SHA-256 hash.
// If the step contains no tool calls, it returns "".
func getToolInteractionSignature(content fantasy.ResponseContent) string {
	toolCalls := content.ToolCalls()
	if len(toolCalls) == 0 {
		return ""
	}

	// Index tool results by their ToolCallID for fast lookup.
	resultsByID := make(map[string]fantasy.ToolResultContent)
	for _, tr := range content.ToolResults() {
		resultsByID[tr.ToolCallID] = tr
	}

	h := sha256.New()
	for _, tc := range toolCalls {
		output := ""
		if tr, ok := resultsByID[tc.ToolCallID]; ok {
			output = toolResultOutputString(tr.Result)
		}
		io.WriteString(h, tc.ToolName)
		io.WriteString(h, "\x00")
		io.WriteString(h, tc.Input)
		io.WriteString(h, "\x00")
		io.WriteString(h, output)
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// toolResultOutputString converts a ToolResultOutputContent to a stable string
// representation for signature comparison.
func toolResultOutputString(result fantasy.ToolResultOutputContent) string {
	if result == nil {
		return ""
	}
	if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result); ok {
		return text.Text
	}
	if errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result); ok {
		if errResult.Error != nil {
			return errResult.Error.Error()
		}
		return ""
	}
	if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result); ok {
		return media.Data
	}
	return ""
}
