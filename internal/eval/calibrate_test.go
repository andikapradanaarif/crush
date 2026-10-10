package eval

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/stretchr/testify/require"
)

func acceptedEp(id int64, session, phrase, target string) CalibrationEpisode {
	return CalibrationEpisode{
		ID: id, SessionID: session, Phrase: phrase, Target: target,
		Verdict: cmdlog.ReferentAccepted,
	}
}

func revisedEp(id int64, session, phrase, target string) CalibrationEpisode {
	return CalibrationEpisode{
		ID: id, SessionID: session, Phrase: phrase, Target: target,
		Verdict: "revised", HashChanged: sql.NullInt64{Int64: 1, Valid: true},
	}
}

func TestHBUpperPValue_KnownValues(t *testing.T) {
	t.Parallel()

	// Zero harms in 60 draws at alpha=0.05: the Hoeffding bound
	// exp(-0.3)≈0.741 is loose; the Bentkus exact tail e·0.95^60
	// ≈0.125 dominates — and stays above delta=0.05, so 60 clean
	// episodes do not certify a 5% bound (the issue's "~60" is a
	// floor, not a guarantee).
	p := hbUpperPValue(0, 60, 0.05)
	require.InDelta(t, 0.1252, p, 1e-3)

	// The crossing point for zero-harm certification at alpha=0.05,
	// delta=0.05 sits at n=78: e·0.95^78 ≈ 0.0496.
	require.Less(t, hbUpperPValue(0, 78, 0.05), 0.05)
	require.Greater(t, hbUpperPValue(0, 77, 0.05), 0.05)

	// An observed rate above alpha can never reject "rate > alpha".
	require.Equal(t, 1.0, hbUpperPValue(5, 10, 0.05))

	// No draws is no evidence.
	require.Equal(t, 1.0, hbUpperPValue(0, 0, 0.05))
}

func TestStratifyOnePerSession(t *testing.T) {
	t.Parallel()

	eps := []CalibrationEpisode{
		acceptedEp(1, "s1", "p", "a"),
		revisedEp(2, "s1", "p", "b"), // same session — dropped
		acceptedEp(3, "s2", "p", "a"),
	}
	got := StratifyOnePerSession(eps)
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[0].ID)
	require.Equal(t, int64(3), got[1].ID)
	// The earliest episode per session is the draw — the later
	// correlated row can't flip the verdict being calibrated on.
}

func TestCalibratePromoteHits_FixedSequence(t *testing.T) {
	t.Parallel()

	// One mapping "fix config"→config.go with 3 clean sessions of
	// acceptance, all outcomes good. At h=5 nothing renders (no
	// admits → p=1 → not certified); the fixed sequence stops there
	// before reaching the honest floors.
	var eps []CalibrationEpisode
	for i := range 3 {
		eps = append(eps, acceptedEp(int64(i+1), fmt.Sprintf("s%d", i), "fix config", "config.go"))
	}
	cert, err := CalibratePromoteHits(eps, []int{1, 2, 3, 4, 5}, 0.05, 0.05)
	require.NoError(t, err)
	require.Nil(t, cert.CertifiedValue)
	require.Len(t, cert.Candidates, 1, "fixed sequence stops at the first non-certifying value")
	require.Equal(t, 5, cert.Candidates[0].Value)
	require.False(t, cert.Candidates[0].Certified)
}

func TestCalibratePromoteHits_CertifiesLeastConservativeSafe(t *testing.T) {
	t.Parallel()

	// Mapping A: 80 clean sessions, all good outcomes — clears every
	// candidate in the grid, admits only good episodes, and is big
	// enough for zero-harm certification at alpha=0.05 (the crossing
	// is n=78 admits; the LOO render floor needs 4+1 clean sessions
	// for the h=3 arm to admit).
	// Mapping B: 1 clean session plus bad-outcome episodes — clears
	// only h=1, and its admits are harms.
	var eps []CalibrationEpisode
	id := int64(1)
	for range 80 {
		eps = append(eps, acceptedEp(id, fmt.Sprintf("a%d", id), "fix config", "config.go"))
		id++
	}
	eps = append(eps, acceptedEp(id, "b0", "fix db", "db.go"))
	id++
	for i := range 2 {
		eps = append(eps, revisedEp(id, fmt.Sprintf("b%d", i+1), "fix db", "db.go"))
		id++
	}

	// Loose bound: the bad admits still certify under alpha=0.9, and
	// the least conservative value in the grid wins.
	cert, err := CalibratePromoteHits(eps, []int{4, 3, 2, 1}, 0.9, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedValue)
	require.Equal(t, 1, *cert.CertifiedValue)
	require.NotNil(t, cert.Overlay)
	require.NotEmpty(t, cert.ParamVersion)

	// Tight bound: mapping B's admit rate is 2/3 bad — under
	// alpha=0.05 no floor admitting B's harms certifies; h=2 is the
	// least conservative value that keeps the admits clean.
	cert, err = CalibratePromoteHits(eps, []int{4, 3, 2, 1}, 0.05, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedValue)
	require.Equal(t, 2, *cert.CertifiedValue)
	last := cert.Candidates[len(cert.Candidates)-1]
	require.Equal(t, 1, last.Value, "the sequence tests down to the first failure")
	require.False(t, last.Certified)
}

func TestCalibratePromoteHits_LeaveOneOutRenderability(t *testing.T) {
	t.Parallel()

	// The mapping's only clean evidence is the episode's own session:
	// at h=1 the episode must NOT count its own acceptance toward
	// rendering itself — deployment-time renderability is evidence
	// that existed before the episode was judged.
	eps := []CalibrationEpisode{acceptedEp(1, "s1", "fix config", "config.go")}
	cert, err := CalibratePromoteHits(eps, []int{1}, 0.9, 0.05)
	require.NoError(t, err)
	require.Nil(t, cert.CertifiedValue)
	require.Equal(t, 0, cert.Candidates[0].Admits)
}

func TestCalibratePromoteHits_HarmUsesSharedOutcome(t *testing.T) {
	t.Parallel()

	// A committed episode counts as success even when the cue verdict
	// was revised — the artifact dominates, identical to what the
	// live rate gate folds.
	mapping := "fix config"
	eps := []CalibrationEpisode{
		acceptedEp(1, "s0", mapping, "config.go"),
		acceptedEp(2, "s1", mapping, "config.go"),
		{
			ID: 3, SessionID: "s2", Phrase: mapping, Target: "config.go",
			Verdict:   "revised",
			Committed: sql.NullInt64{Int64: 1, Valid: true},
		},
	}
	cert, err := CalibratePromoteHits(eps, []int{1}, 0.9, 0.05)
	require.NoError(t, err)
	require.Equal(t, 0, cert.Candidates[0].Harms, "committed is success under the shared outcome rule")
}

func TestCalibratePromoteHits_Validation(t *testing.T) {
	t.Parallel()

	_, err := CalibratePromoteHits(nil, nil, 0.05, 0.05)
	require.Error(t, err)
	_, err = CalibratePromoteHits(nil, []int{1}, 0, 0.05)
	require.Error(t, err)
	_, err = CalibratePromoteHits(nil, []int{1}, 0.05, 1.5)
	require.Error(t, err)
}
