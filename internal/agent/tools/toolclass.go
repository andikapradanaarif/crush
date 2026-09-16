package tools

import (
	"encoding/json"

	"github.com/charmbracelet/crush/internal/toolclass"
)

// The classification vocabulary lives in the internal/toolclass leaf
// package so the notebook layer can share it without importing the
// whole tool set — these aliases keep the tools.* call sites and the
// tools.IsMutatingCall vocabulary path stable.
var (
	WriteToolNames   = toolclass.WriteToolNames
	ReadToolNames    = toolclass.ReadToolNames
	CommandToolNames = toolclass.CommandToolNames
)

// IsMutatingCall classifies a call as a write for boundary purposes.
func IsMutatingCall(name, input string) bool {
	return toolclass.IsMutatingCall(name, input)
}

// ToolCallFilePath extracts the file path from a tool call's JSON
// input, trying the conventional keys.
func ToolCallFilePath(input string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "file"} {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
