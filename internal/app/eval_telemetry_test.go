package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// stubTelemetryCoordinator embeds a nil Coordinator — only the
// SessionTelemetry assertion in emitEvalTelemetry ever calls into it.
type stubTelemetryCoordinator struct {
	agent.Coordinator
	tel agent.SessionTelemetry
}

func (s stubTelemetryCoordinator) SessionTelemetry(string) agent.SessionTelemetry {
	return s.tel
}

func telemetryApp(tel agent.SessionTelemetry) *App {
	return &App{
		AgentCoordinator: stubTelemetryCoordinator{tel: tel},
		config:           config.NewTestStore(&config.Config{Options: &config.Options{}}),
	}
}

func readTelemetryDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	return doc
}

// The ledger counts every model invocation; result.TotalUsage covers
// only whichever call returned last. When the ledger has usage it
// must win — this is the fix for continuation/summarize spend going
// uncounted.
func TestEmitEvalTelemetry_PrefersLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.json")
	t.Setenv(EvalTelemetryEnvVar, path)

	app := telemetryApp(agent.SessionTelemetry{
		LedgerUsage: fantasy.Usage{InputTokens: 500, OutputTokens: 40, CacheReadTokens: 12},
		LedgerSteps: 9,
	})
	result := &fantasy.AgentResult{
		TotalUsage: fantasy.Usage{InputTokens: 100, OutputTokens: 10},
		Steps:      make([]fantasy.StepResult, 3),
	}
	app.emitEvalTelemetry("sess", result, nil, 0)

	doc := readTelemetryDoc(t, path)
	tokens, ok := doc["tokens"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(500), tokens["input"])
	require.Equal(t, float64(40), tokens["output"])
	require.Equal(t, float64(12), tokens["cache_read"])
	require.Equal(t, float64(9), doc["steps"])
}

// A session with no ledger usage (e.g. the run failed before any
// model call completed) falls back to the returned result, then to
// the approximate step count for killed runs.
func TestEmitEvalTelemetry_LedgerFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.json")
	t.Setenv(EvalTelemetryEnvVar, path)

	app := telemetryApp(agent.SessionTelemetry{})
	result := &fantasy.AgentResult{
		TotalUsage: fantasy.Usage{InputTokens: 100, OutputTokens: 10},
		Steps:      make([]fantasy.StepResult, 3),
	}
	app.emitEvalTelemetry("sess", result, nil, 7)

	doc := readTelemetryDoc(t, path)
	tokens, ok := doc["tokens"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(100), tokens["input"])
	require.Equal(t, float64(3), doc["steps"])

	// No result at all — the approximate count from observed
	// assistant messages is the last resort.
	path2 := filepath.Join(t.TempDir(), "tel2.json")
	t.Setenv(EvalTelemetryEnvVar, path2)
	app.emitEvalTelemetry("sess", nil, nil, 7)
	doc = readTelemetryDoc(t, path2)
	require.Equal(t, float64(7), doc["steps"])
}

// The per-Run audit history must reach the telemetry file — the
// coordinator populates TailRuns but the emission stanza was missing,
// so powered-run records carried `tail` but never `tail_runs` and a
// retry chain's renders stayed flattened into last-write-wins.
func TestEmitEvalTelemetry_TailRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tel.json")
	t.Setenv(EvalTelemetryEnvVar, path)

	app := telemetryApp(agent.SessionTelemetry{
		Tail: &agent.TailAudit{Bytes: 10, RunStamp: 1},
		TailRuns: []agent.TailAudit{
			{Bytes: 10, RunStamp: 1},
			{Bytes: 20, RunStamp: 1, RepairAttempts: 1},
		},
	})
	app.emitEvalTelemetry("sess", nil, nil, 0)

	doc := readTelemetryDoc(t, path)
	runs, ok := doc["tail_runs"].([]any)
	require.True(t, ok, "tail_runs missing from telemetry doc")
	require.Len(t, runs, 2)
	second, ok := runs[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1), second["repair_attempts"])

	// Absent rather than null when no Runs rendered a tail.
	path2 := filepath.Join(t.TempDir(), "tel2.json")
	t.Setenv(EvalTelemetryEnvVar, path2)
	telemetryApp(agent.SessionTelemetry{}).emitEvalTelemetry("sess", nil, nil, 0)
	require.NotContains(t, readTelemetryDoc(t, path2), "tail_runs")
}

func TestClassifyRunError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"context cancelled", context.Canceled, "cancelled"},
		{"wrapped cancelled", fmt.Errorf("run: %w", context.Canceled), "cancelled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"auth status", &fantasy.ProviderError{StatusCode: 401, Message: "unauthorized"}, "auth"},
		{"auth flag without status", &fantasy.ProviderError{AuthError: true, Message: "token expired"}, "auth"},
		{"forbidden", &fantasy.ProviderError{StatusCode: 403}, "auth"},
		{"context too large", &fantasy.ProviderError{StatusCode: 400, ContextTooLargeErr: true}, "context_too_large"},
		{"enforced window cap", fmt.Errorf("run: %w", agent.ErrContextWindowExceeded), "window_cap_enforced"},
		{"context too large via tokens", &fantasy.ProviderError{ContextMaxTokens: 128000, ContextUsedTokens: 200000}, "context_too_large"},
		{"rate limit", &fantasy.ProviderError{StatusCode: 429}, "rate_limit"},
		{"request timeout retryable", &fantasy.ProviderError{StatusCode: 408}, "provider_transient"},
		{"conflict retryable", &fantasy.ProviderError{StatusCode: 409}, "provider_transient"},
		{"deterministic 4xx", &fantasy.ProviderError{StatusCode: 422, Message: "schema rejected"}, "provider_deterministic"},
		{"server error", &fantasy.ProviderError{StatusCode: 503}, "provider_server"},
		{"connection refused", &fantasy.ProviderError{Cause: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, "provider_unreachable"},
		{"dns failure", &fantasy.ProviderError{Cause: &net.DNSError{Err: "no such host", IsNotFound: true}}, "provider_unreachable"},
		{"transient flag", &fantasy.ProviderError{TransientError: true, Message: "mid-stream reset"}, "provider_transient"},
		{"incomplete stream", fantasy.NewIncompleteStreamError(), "provider_transient"},
		{"untyped provider error", &fantasy.ProviderError{Message: "weird"}, "provider_other"},
		{"retry-wrapped auth", &fantasy.RetryError{Errors: []error{
			&fantasy.ProviderError{StatusCode: 401},
		}}, "auth"},
		// Transport failures reaching us bare — RetryError.Unwrap
		// yields the last attempt error without a ProviderError wrap,
		// the live shape the Alibaba endpoint produced on outages.
		{"retry-wrapped url timeout", &fantasy.RetryError{Errors: []error{
			// The real shape: url.Error wrapping a net.Error with
			// Timeout() — TLS handshake timeouts surface as
			// http.tlsHandshakeTimeoutError, unexported, so
			// ETIMEDOUT stands in for the Timeout()=true chain.
			&url.Error{Op: "Post", URL: "https://x/v1/chat/completions", Err: &os.SyscallError{Syscall: "read", Err: syscall.ETIMEDOUT}},
		}}, "provider_transient"},
		{"retry-wrapped refused", &fantasy.RetryError{Errors: []error{
			&url.Error{Op: "Post", URL: "https://x", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
		}}, "provider_unreachable"},
		{"bare url error", &url.Error{Op: "Post", URL: "https://x", Err: io.ErrUnexpectedEOF}, "provider_transient"},
		{"bare net op timeout", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}}, "provider_transient"},
		// Non-retryable transport shapes match the ProviderError branch
		// — cert verification failure is provider_other, not transient.
		{"bare cert failure", &url.Error{Op: "Post", URL: "https://x", Err: x509.UnknownAuthorityError{}}, "provider_other"},
		{"non-provider error", errors.New("disk full"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, classifyRunError(tc.err))
		})
	}
}
