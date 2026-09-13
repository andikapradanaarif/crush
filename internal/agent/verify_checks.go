package agent

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
)

// sourceFileExts are the file extensions the check selector treats as
// source code. Non-source files (docs, config, data) select no gate
// checks.
var sourceFileExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".py": true, ".rs": true, ".c": true,
	".cc": true, ".cpp": true, ".h": true, ".hpp": true, ".java": true,
	".rb": true, ".php": true, ".cs": true, ".swift": true, ".kt": true,
	".kts": true, ".m": true, ".scala": true, ".ex": true, ".exs": true,
	".erl": true, ".hrl": true, ".hs": true, ".clj": true, ".lua": true,
}

// pendingChecksForEdit returns the gate-run checks a mutation to absPath
// selects: the configured verify commands when no LSP covers the file
// (the diagnostics delta is the LSP-covered compile check), plus a
// same-package test for Go files in a tested directory. The decorator
// records the result as pending entries in the verification metadata.
func pendingChecksForEdit(cfg *config.Config, workingDir, absPath string, lspCovered bool) []message.VerificationCheck {
	ext := strings.ToLower(filepath.Ext(absPath))
	if cfg == nil || !sourceFileExts[ext] {
		return nil
	}
	var checks []message.VerificationCheck
	if !lspCovered {
		for _, v := range cfg.Verify {
			checks = append(checks, message.VerificationCheck{
				Check:   "verify:" + v.DisplayName(),
				State:   message.VerificationPending,
				Command: v.Command,
				Timeout: int(v.TimeoutDuration() / time.Second),
			})
		}
	}
	if ext == ".go" {
		dir := filepath.Dir(absPath)
		rel, err := filepath.Rel(workingDir, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && dirHasGoTestFile(dir) {
			checks = append(checks, message.VerificationCheck{
				Check:   "package-test:" + rel,
				State:   message.VerificationPending,
				Command: "go test ./" + rel,
				Timeout: 120,
			})
		}
	}
	return checks
}

// dirHasGoTestFile reports whether dir contains a *_test.go file.
func dirHasGoTestFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			return true
		}
	}
	return false
}
