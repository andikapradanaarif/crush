package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"charm.land/fantasy"
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
		{"context too large via tokens", &fantasy.ProviderError{ContextMaxTokens: 128000, ContextUsedTokens: 200000}, "context_too_large"},
		{"rate limit", &fantasy.ProviderError{StatusCode: 429}, "rate_limit"},
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
		{"non-provider error", errors.New("disk full"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, classifyRunError(tc.err))
		})
	}
}
