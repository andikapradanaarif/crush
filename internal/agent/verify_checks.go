package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
)

// pendingChecksForEdit returns the gate-run checks a mutation to absPath
// selects: every configured verify command (a declared project check
// gates ANY file mutation — a dependency or manifest edit breaks builds
// as readily as source does), plus a same-package test for Go files in a
// tested directory. The decorator records the result as pending entries
// in the verification metadata.
func pendingChecksForEdit(cfg *config.Config, workingDir, absPath string) []message.VerificationCheck {
	if cfg == nil {
		return nil
	}
	var checks []message.VerificationCheck
	for _, v := range cfg.Verify {
		checks = append(checks, message.VerificationCheck{
			Check:   "verify:" + v.DisplayName(),
			State:   message.VerificationPending,
			Command: v.Command,
			Timeout: int(v.TimeoutDuration() / time.Second),
		})
	}
	if strings.EqualFold(filepath.Ext(absPath), ".go") {
		dir := filepath.Dir(absPath)
		rel, err := filepath.Rel(workingDir, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && dirHasGoTestFile(dir) {
			checks = append(checks, message.VerificationCheck{
				Check:   "package-test:" + rel,
				State:   message.VerificationPending,
				Command: fmt.Sprintf("go test %q", "./"+rel),
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
