package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticsSnapshot_NewErrorsSince(t *testing.T) {
	t.Parallel()

	baseline := DiagnosticsSnapshot{
		"a.go|1|1|undefined: x": 1,
		"b.go|2|2|old error":    2,
	}
	after := DiagnosticsSnapshot{
		"a.go|1|1|undefined: x":   1, // unchanged
		"b.go|2|2|old error":      2, // unchanged
		"c.go|3|3|new error":      1, // brand new
		"b.go|2|2|old error twin": 0, // absent keys contribute nothing
		"a.go|5|5|new also":       2, // count beyond baseline
	}

	newErrs := after.NewErrorsSince(baseline)
	require.ElementsMatch(t, []string{
		"c.go|3|3|new error",
		"a.go|5|5|new also",
		"a.go|5|5|new also",
	}, newErrs, "delta is a multiset: extra counts surface once each")
}

func TestDiagnosticsSnapshot_NewErrorsSince_RemovalOnly(t *testing.T) {
	t.Parallel()

	baseline := DiagnosticsSnapshot{"a.go|1|1|err": 2}
	after := DiagnosticsSnapshot{"a.go|1|1|err": 1}

	require.Empty(t, after.NewErrorsSince(baseline),
		"fewer diagnostics than baseline must not report new errors")
}

func TestSnapshotDiagnostics_NilManager(t *testing.T) {
	t.Parallel()

	require.Empty(t, SnapshotDiagnostics(nil))
}

func TestAnyClientHandles(t *testing.T) {
	t.Parallel()

	require.False(t, AnyClientHandles(nil, "/tmp/x.go"))
	require.False(t, AnyClientHandles(nil, ""))
}
