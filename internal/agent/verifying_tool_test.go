package agent

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestWrapToolsWithVerification(t *testing.T) {
	t.Parallel()

	inputs := []fantasy.AgentTool{
		&fakeTool{name: "edit"},
		&fakeTool{name: "write"},
		&fakeTool{name: "multiedit"},
		&fakeTool{name: "lsp_rename"},
		&fakeTool{name: "lsp_replace_symbol"},
		&fakeTool{name: "bash"},
		&fakeTool{name: "view"},
	}

	out := wrapToolsWithVerification(inputs, nil, t.TempDir(), nil)
	require.Len(t, out, len(inputs))
	for i, tool := range inputs {
		wrapped, isWrapped := out[i].(*verifyingTool)
		require.Equal(t, tools.WriteToolNames[tool.Info().Name], isWrapped,
			"tool %q wrap = %v, want %v", tool.Info().Name, isWrapped, tools.WriteToolNames[tool.Info().Name])
		if isWrapped && tool.Info().Name == "lsp_rename" {
			require.True(t, wrapped.projectWide, "rename refreshes all open files")
		}
	}
}

func TestVerifyingTool_UnverifiedWhenNoClientHandlesFile(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("ok")}
	tool := &verifyingTool{inner: inner, lspManager: nil, workingDir: t.TempDir()}

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  "edit",
		Input: `{"file_path":"/tmp/x.go","old_string":"a","new_string":"b"}`,
	})
	require.NoError(t, err)
	require.True(t, inner.called)

	var meta struct {
		Verification []message.VerificationCheck `json:"verification"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Len(t, meta.Verification, 1)
	require.Equal(t, "diagnostics", meta.Verification[0].Check)
	require.Equal(t, message.VerificationUnverified, meta.Verification[0].State)
	require.Equal(t, filepathext.Canonical("/tmp/x.go"), meta.Verification[0].Path)
}

func TestVerifyingTool_ErrorResponseSkipsVerification(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "edit", resp: fantasy.NewTextErrorResponse("edit failed")}
	tool := &verifyingTool{inner: inner, lspManager: nil, workingDir: t.TempDir()}

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  "edit",
		Input: `{"file_path":"/tmp/x.go"}`,
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Empty(t, resp.Metadata, "failed mutation must not carry verification metadata")
}

func TestVerifyingTool_MissingPathPassesThrough(t *testing.T) {
	t.Parallel()

	inner := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("ok")}
	tool := &verifyingTool{inner: inner, lspManager: nil, workingDir: t.TempDir()}

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  "edit",
		Input: `{"old_string":"a"}`,
	})
	require.NoError(t, err)
	require.True(t, inner.called)
	require.Empty(t, resp.Metadata)
}

// TestVerifyingTool_ProjectWideNoPath pins that a workspace-edit tool
// with no usable anchor path still runs the verification flow instead
// of passing through — a rename's `path` is an optional search root,
// not a written file.
func TestVerifyingTool_ProjectWideNoPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inner := &fakeTool{name: "lsp_rename", resp: fantasy.NewTextResponse("ok")}
	tool := &verifyingTool{inner: inner, lspManager: nil, workingDir: dir, projectWide: true}

	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  "lsp_rename",
		Input: `{"symbol":"Old","new_name":"New"}`,
	})
	require.NoError(t, err)
	require.True(t, inner.called)

	var meta struct {
		Verification []message.VerificationCheck `json:"verification"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Len(t, meta.Verification, 1)
	require.Equal(t, "diagnostics", meta.Verification[0].Check)
	require.Equal(t, message.VerificationUnverified, meta.Verification[0].State)
	require.Equal(t, filepathext.Canonical(dir), meta.Verification[0].Path)
}

func TestDiagnosticsChecks(t *testing.T) {
	t.Parallel()

	t.Run("failed delta mints per-file entries", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationFailed, "2 new error(s)", map[string]int{
			"/w/b.go": 2,
		}, nil)
		require.Len(t, checks, 2)
		require.Equal(t, "diagnostics", checks[0].Check)
		require.Equal(t, "/w/a.go", checks[0].Path)
		require.Equal(t, message.VerificationPassed, checks[0].State,
			"the write path stays green when the delta broke another file")
		require.Equal(t, "/w/b.go", checks[1].Path)
		require.Equal(t, message.VerificationFailed, checks[1].State)
		require.Equal(t, "2 new error(s)", checks[1].Detail)
	})

	t.Run("write path failure mints a single entry", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationFailed, "1 new error(s)", map[string]int{
			"/w/a.go": 1,
		}, nil)
		require.Len(t, checks, 1)
		require.Equal(t, "/w/a.go", checks[0].Path)
		require.Equal(t, message.VerificationFailed, checks[0].State)
	})

	t.Run("multiple broken files sort stably", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationFailed, "3 new error(s)", map[string]int{
			"/w/c.go": 2,
			"/w/b.go": 1,
		}, nil)
		require.Len(t, checks, 3)
		require.Equal(t, "/w/b.go", checks[1].Path)
		require.Equal(t, "/w/c.go", checks[2].Path)
	})

	t.Run("resolved files mint passed entries", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationPassed, "", nil, []string{"/w/b.go"})
		require.Len(t, checks, 2)
		require.Equal(t, "/w/a.go", checks[0].Path)
		require.Equal(t, "/w/b.go", checks[1].Path)
		require.Equal(t, message.VerificationPassed, checks[1].State)
		require.Equal(t, "errors resolved", checks[1].Detail)
	})

	t.Run("resolved write path does not duplicate its entry", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationPassed, "", nil, []string{"/w/a.go"})
		require.Len(t, checks, 1)
		require.Equal(t, "/w/a.go", checks[0].Path)
	})

	t.Run("non-failed state mints one write-path entry", func(t *testing.T) {
		t.Parallel()
		checks := diagnosticsChecks("/w/a.go", message.VerificationUnverified, "did not settle", nil, nil)
		require.Len(t, checks, 1)
		require.Equal(t, message.VerificationUnverified, checks[0].State)
		require.Equal(t, "/w/a.go", checks[0].Path)
	})
}

func TestMergeVerificationMetadata_ComposesWithHookKey(t *testing.T) {
	t.Parallel()

	existing := `{"hook":{"hook_count":1,"decision":"allow"}}`
	merged := mergeVerificationMetadata(existing, []message.VerificationCheck{{
		Check: "diagnostics",
		State: message.VerificationPassed,
	}})

	var meta struct {
		Hook         map[string]any              `json:"hook"`
		Verification []message.VerificationCheck `json:"verification"`
	}
	require.NoError(t, json.Unmarshal([]byte(merged), &meta))
	require.Equal(t, float64(1), meta.Hook["hook_count"], "hook key must survive the verification merge")
	require.Len(t, meta.Verification, 1)
	require.Equal(t, message.VerificationPassed, meta.Verification[0].State)
}

func TestMergeVerificationMetadata_EmptyExisting(t *testing.T) {
	t.Parallel()

	merged := mergeVerificationMetadata("", []message.VerificationCheck{{
		Check: "diagnostics",
		State: message.VerificationFailed,
	}})

	var meta struct {
		Verification []message.VerificationCheck `json:"verification"`
	}
	require.NoError(t, json.Unmarshal([]byte(merged), &meta))
	require.Len(t, meta.Verification, 1)
	require.Equal(t, message.VerificationFailed, meta.Verification[0].State)
}
