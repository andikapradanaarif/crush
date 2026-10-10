package cmdlog

import (
	"database/sql"
	"math"
	"strconv"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// rateEnv wires a service over a real migrated DB with a pinned
// project key, so several services can share one ledger for the
// isolation test (#296).
type rateEnv struct {
	svc  Service
	conn *sql.DB
}

func setupRatesTest(t *testing.T, projectKey string) *rateEnv {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return &rateEnv{
		svc: NewService(db.New(conn), t.TempDir(), params.DefaultMemory(),
			WithProjectKey(projectKey)),
		conn: conn,
	}
}

// seedRateEpisode inserts one referent_episodes row straight into
// the ledger — the fold reads settled outcomes, so tests need only
// the verdict and label columns, not the full episode pipeline.
func seedRateEpisode(t *testing.T, env *rateEnv, projectKey, session, msg, verdict string,
	committed, hashChanged sql.NullInt64,
) {
	t.Helper()
	_, err := env.conn.ExecContext(t.Context(),
		`INSERT INTO referent_episodes
		 (phrase, target, session_id, source_message_id, tool_call_id,
		  repo_state, memory_suggested, verdict, project_key,
		  param_version, created_at, label_committed, label_hash_changed)
		 VALUES ('p', 'f.go', ?, ?, 'c', '', 0, ?, ?, '', 1, ?, ?)`,
		session, msg, verdict, projectKey, committed, hashChanged)
	require.NoError(t, err)
}

// itoa formats small ints for unique message IDs — strconv.Itoa by
// another name, kept so seed call sites stay one-liners.
func itoa(i int) string {
	return strconv.Itoa(i)
}

func storedRate(t *testing.T, env *rateEnv, projectKey string) (alpha, beta float64, lastSession string) {
	t.Helper()
	var ls string
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT alpha, beta, last_session_id FROM project_rates
		 WHERE project_key = ? AND signal = 'referent'`, projectKey).
		Scan(&alpha, &beta, &ls))
	return alpha, beta, ls
}

// Issue #296's first target: a project with zero events decides on
// the global prior alone — no local evidence means the posterior IS
// the prior, not a fabricated optimism.
func TestUpdateProjectRates_ZeroEventsPriorDecision(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	// An empty corpus folds nothing and yields Beta(1,1) — uniform.
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
	bound, err := env.svc.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	require.InDelta(t, 0.1, bound, 1e-6)
	// With global evidence but no local evidence, the project still
	// decides on the prior — now the pooled one.
	for i := range 8 {
		seedRateEpisode(t, env, "other", "s", "m"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	bound, err = env.svc.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	// Prior = Beta(1+8, 1) — the project itself never folded.
	require.InDelta(t, math.Pow(0.1, 1.0/9), bound, 1e-6)
}

// Issue #296's second target: ~20 events visibly move the bound off
// the prior.
func TestUpdateProjectRates_TwentyEventsDeparture(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	for i := range 20 {
		seedRateEpisode(t, env, "pk", "s1", "m"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
	bound, err := env.svc.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	// Leave-one-out prior Beta(1,1) + local 20 = posterior
	// Beta(21,1), q0.1 = 0.1^(1/21).
	require.Greater(t, bound, 0.8)
	require.InDelta(t, math.Pow(0.1, 1.0/21), bound, 1e-6)
}

// Outcome direction: successes raise the bound, failures sink it.
func TestUpdateProjectRates_OutcomeDirections(t *testing.T) {
	t.Parallel()
	boundFor := func(accepted, revised int) float64 {
		env := setupRatesTest(t, "pk")
		for i := range accepted {
			seedRateEpisode(t, env, "pk", "s1", "a"+itoa(i), ReferentAccepted,
				sql.NullInt64{}, sql.NullInt64{})
		}
		for i := range revised {
			seedRateEpisode(t, env, "pk", "s1", "r"+itoa(i), ReferentRevised,
				sql.NullInt64{}, sql.NullInt64{})
		}
		require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
		b, err := env.svc.ReferentRateLowerBound(t.Context(), 0.1)
		require.NoError(t, err)
		return b
	}
	high := boundFor(20, 0)
	mid := boundFor(10, 10)
	low := boundFor(0, 20)
	require.Greater(t, high, mid)
	require.Greater(t, mid, low)
	// The mixed case lands on Beta(11,11)'s lower tail — the
	// leave-one-out prior adds no mass of its own here.
	require.InDelta(t, boundFor(10, 10), BetaQuantile(11, 11, 0.1), 1e-6)
}

// Decay lands once per distinct session — both within one fold and
// across fold boundaries (the last_session_id cursor).
func TestUpdateProjectRates_DecayAcrossSessions(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	// Session s1: 10 accepted. Fold, then a second session's events.
	for i := range 10 {
		seedRateEpisode(t, env, "pk", "s1", "a"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.5))
	alpha, beta, last := storedRate(t, env, "pk")
	require.InDelta(t, 10, alpha, 1e-9)
	require.Zero(t, beta)
	require.Equal(t, "s1", last)
	// Session s2: 10 more. The s1 mass discounts once — alpha must be
	// 10*0.5 + 10, not 20.
	for i := range 10 {
		seedRateEpisode(t, env, "pk", "s2", "b"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.5))
	alpha, _, last = storedRate(t, env, "pk")
	require.InDelta(t, 15, alpha, 1e-9)
	require.Equal(t, "s2", last)
	// A third fold with no new events is a no-op — the cursor holds.
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.5))
	alpha, _, _ = storedRate(t, env, "pk")
	require.InDelta(t, 15, alpha, 1e-9)
}

// Interleaved sessions still decay once per DISTINCT session.
func TestUpdateProjectRates_InterleavedSessions(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	seedRateEpisode(t, env, "pk", "A", "1", ReferentAccepted, sql.NullInt64{}, sql.NullInt64{})
	seedRateEpisode(t, env, "pk", "B", "2", ReferentAccepted, sql.NullInt64{}, sql.NullInt64{})
	seedRateEpisode(t, env, "pk", "A", "3", ReferentAccepted, sql.NullInt64{}, sql.NullInt64{})
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.5))
	// A folds 2 undiscounted, B decays once and folds 1: 2*0.5 + 1.
	alpha, _, _ := storedRate(t, env, "pk")
	require.InDelta(t, 2, alpha, 1e-9)
}

// The outcome rule — artifact signals dominate the cue verdict.
func TestReferentEpisodeOutcome(t *testing.T) {
	t.Parallel()
	yes := sql.NullInt64{Int64: 1, Valid: true}
	no := sql.NullInt64{Int64: 0, Valid: true}
	null := sql.NullInt64{}
	cases := []struct {
		name      string
		verdict   string
		committed sql.NullInt64
		changed   sql.NullInt64
		want      bool
	}{
		{"commit beats divergence", ReferentRevised, yes, yes, true},
		{"commit beats survival", ReferentAccepted, yes, no, true},
		{"commit alone", ReferentUnknown, yes, null, true},
		{"divergence uncommitted", ReferentAccepted, null, yes, false},
		{"survival uncommitted", ReferentUnknown, null, no, true},
		{"not committed survives", ReferentUnknown, sql.NullInt64{Int64: 0, Valid: true}, no, true},
		{"accepted verdict", ReferentAccepted, null, null, true},
		{"revised verdict", ReferentRevised, null, null, false},
		{"unknown verdict", ReferentUnknown, null, null, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.want, referentEpisodeOutcome(c.verdict, c.committed, c.changed))
		})
	}
}

// Settledness: an unresolved episode never enters the event stream.
func TestUpdateProjectRates_UnsettledSkipped(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	seedRateEpisode(t, env, "pk", "s1", "1", ReferentUnknown,
		sql.NullInt64{}, sql.NullInt64{})
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
	var n int
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM project_rates`).Scan(&n))
	require.Zero(t, n)
}

// Project isolation under leave-one-out: each project's prior
// pools everyone ELSE, so synchronous posteriors coincide at the
// pool total — divergence arrives only through decay. pa's 20
// successes spread across two sessions fold as discounted mass
// (15 at γ=0.5), while pb's prior sees all 20 undecayed: pa's own
// stale evidence counts LESS for pa than it does for a borrower.
func TestReferentRateLowerBound_ProjectIsolation(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pa")
	other := NewService(db.New(env.conn), t.TempDir(), params.DefaultMemory(),
		WithProjectKey("pb"))
	third := NewService(db.New(env.conn), t.TempDir(), params.DefaultMemory(),
		WithProjectKey("pc"))
	for i := range 10 {
		seedRateEpisode(t, env, "pa", "s1", "a"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	for i := range 10 {
		seedRateEpisode(t, env, "pa", "s2", "b"+itoa(i), ReferentAccepted,
			sql.NullInt64{}, sql.NullInt64{})
	}
	for i := range 5 {
		seedRateEpisode(t, env, "pc", "s1", "f"+itoa(i), ReferentRevised,
			sql.NullInt64{}, sql.NullInt64{})
	}
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.5))
	require.NoError(t, other.UpdateProjectRates(t.Context(), 0.5))
	require.NoError(t, third.UpdateProjectRates(t.Context(), 0.5))
	paBound, err := env.svc.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	pbBound, err := other.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	pcBound, err := third.ReferentRateLowerBound(t.Context(), 0.1)
	require.NoError(t, err)
	// pa: LOO prior pools pc's 5 failures only → Beta(1,6); local
	// mass is session-decayed: 10·0.5 + 10 = 15 → Beta(16,6).
	require.InDelta(t, BetaQuantile(16, 6, 0.1), paBound, 1e-6)
	// pb has no local mass: its prior is the whole pool Beta(21,6).
	require.InDelta(t, BetaQuantile(21, 6, 0.1), pbBound, 1e-6)
	// pc: LOO prior pools pa's 20 → Beta(21,1) + local 5 failures.
	require.InDelta(t, BetaQuantile(21, 6, 0.1), pcBound, 1e-6)
	// pa's decayed evidence pulls its own bound below the pooled
	// view pb borrows — drift-tracking is the point of the layer.
	require.Less(t, paBound, pbBound)
}

// The fold's membership is the rate_folded mark, not an id
// watermark: a row whose labels settle AFTER higher ids folded
// still counts when the maturity pass resolves it (#296 review —
// the artifact-evidenced population can't be skipped).
func TestUpdateProjectRates_LateSettlingRowFolds(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	// Unknown verdict, no labels yet — unsettled at fold time.
	seedRateEpisode(t, env, "pk", "s1", "early", ReferentUnknown,
		sql.NullInt64{}, sql.NullInt64{})
	// A higher-id row that IS settled folds first.
	seedRateEpisode(t, env, "pk", "s2", "late", ReferentAccepted,
		sql.NullInt64{}, sql.NullInt64{})
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
	alpha, beta, _ := storedRate(t, env, "pk")
	require.InDelta(t, 1, alpha, 1e-9)
	require.Zero(t, beta)
	// Maturity resolves the earlier row: hash still matches.
	_, err := env.conn.ExecContext(t.Context(),
		`UPDATE referent_episodes SET label_hash_changed = 0
		 WHERE source_message_id = 'early'`)
	require.NoError(t, err)
	require.NoError(t, env.svc.UpdateProjectRates(t.Context(), 0.95))
	alpha, _, _ = storedRate(t, env, "pk")
	// The late-settled row folds — discounted once for being a new
	// session: 1*0.95 + 1, not 2.
	require.InDelta(t, 1.95, alpha, 1e-9)
}

func TestUpdateProjectRates_GammaBounds(t *testing.T) {
	t.Parallel()
	env := setupRatesTest(t, "pk")
	for _, g := range []float64{0, -0.1, 1, 1.5} {
		require.Error(t, env.svc.UpdateProjectRates(t.Context(), g))
	}
}
