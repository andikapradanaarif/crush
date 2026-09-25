package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/stretchr/testify/require"
)

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
			&url.Error{Op: "Post", URL: "https://x/v1/chat/completions", Err: errors.New("net/http: TLS handshake timeout")},
		}}, "provider_transient"},
		{"retry-wrapped refused", &fantasy.RetryError{Errors: []error{
			&url.Error{Op: "Post", URL: "https://x", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
		}}, "provider_unreachable"},
		{"bare url error", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("EOF")}, "provider_transient"},
		{"bare net op timeout", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}}, "provider_transient"},
		{"non-provider error", errors.New("disk full"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, classifyRunError(tc.err))
		})
	}
}
