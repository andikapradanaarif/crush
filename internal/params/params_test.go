package params

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryVersion(t *testing.T) {
	t.Parallel()

	t.Run("stable and distinct from pv0", func(t *testing.T) {
		t.Parallel()
		v := DefaultMemory().Version()
		require.True(t, strings.HasPrefix(v, "pv1-"), v)
		require.NotEqual(t, "pv0", v)
		require.Equal(t, v, DefaultMemory().Version(), "version must be deterministic")
	})

	t.Run("tracks the values", func(t *testing.T) {
		t.Parallel()
		p := DefaultMemory()
		p.OpenRenderLimit++
		require.NotEqual(t, DefaultMemory().Version(), p.Version(),
			"a changed value must produce a different snapshot identity")
	})
}

func TestMemoryOrDefault(t *testing.T) {
	t.Parallel()
	require.Equal(t, DefaultMemory(), Memory{}.OrDefault())
	p := DefaultMemory()
	p.FetchLimit = 7
	require.Equal(t, p, p.OrDefault(), "a nonzero set resolves to itself")
}

func TestResolveMemory(t *testing.T) {
	t.Parallel()

	t.Run("empty overlay resolves defaults", func(t *testing.T) {
		t.Parallel()
		for _, overlay := range []map[string]any{nil, {}} {
			m, err := ResolveMemory(overlay)
			require.NoError(t, err)
			require.Equal(t, DefaultMemory(), m)
		}
	})

	t.Run("overlay applies and re-versions", func(t *testing.T) {
		t.Parallel()
		m, err := ResolveMemory(map[string]any{
			"open_render_limit": 8,
			"open_failure_ttl":  "168h",
		})
		require.NoError(t, err)
		require.Equal(t, 8, m.OpenRenderLimit)
		require.Equal(t, 168*time.Hour, m.OpenFailureTTL)
		require.Equal(t, DefaultMemory().FetchLimit, m.FetchLimit, "unmentioned keys stay at default")
		require.NotEqual(t, DefaultMemory().Version(), m.Version())
	})

	t.Run("ttl accepts seconds", func(t *testing.T) {
		t.Parallel()
		m, err := ResolveMemory(map[string]any{"open_failure_ttl": 3600 * 48})
		require.NoError(t, err)
		require.Equal(t, 48*time.Hour, m.OpenFailureTTL)
	})

	t.Run("ttl zero disables", func(t *testing.T) {
		t.Parallel()
		m, err := ResolveMemory(map[string]any{"open_failure_ttl": "0s"})
		require.NoError(t, err)
		require.Equal(t, time.Duration(0), m.OpenFailureTTL)
	})

	t.Run("unknown key fails", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveMemory(map[string]any{"open_rennder_limit": 8})
		require.Error(t, err)
	})

	t.Run("wrong type fails", func(t *testing.T) {
		t.Parallel()
		for _, overlay := range []map[string]any{
			{"open_render_limit": "eight"},
			{"open_render_limit": 1.5},
			{"open_failure_ttl": true},
			{"open_failure_ttl": "about a week"},
		} {
			_, err := ResolveMemory(overlay)
			require.Error(t, err, "overlay %v", overlay)
		}
	})

	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		for _, ttl := range []any{"30m", "720h0m1s", float64(60), float64(720*3600 + 1)} {
			_, err := ResolveMemory(map[string]any{"open_failure_ttl": ttl})
			require.Error(t, err, "ttl %v outside [24h, 720h] must fail", ttl)
		}
		_, err := ResolveMemory(map[string]any{"open_failure_ttl": "24h"})
		require.NoError(t, err, "lower bound is inclusive")
		_, err = ResolveMemory(map[string]any{"open_failure_ttl": "720h"})
		require.NoError(t, err, "upper bound is inclusive")
	})

	t.Run("negative caps fail", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveMemory(map[string]any{"fetch_limit": -1})
		require.Error(t, err)
	})

	t.Run("zero caps allowed (tighten direction)", func(t *testing.T) {
		t.Parallel()
		m, err := ResolveMemory(map[string]any{"open_render_limit": 0})
		require.NoError(t, err)
		require.Equal(t, 0, m.OpenRenderLimit)
	})
}
