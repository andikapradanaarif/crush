package eval

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// boundDec builds one unit's decisions in render order: bound non-
// excluded rows, then a tail of (bound, excluded) pairs. The unit
// is the draw — several decisions under one name are one
// correlated observation.
func boundDec(unit, class string, bound, excluded int) []CalibrationDecision {
	var out []CalibrationDecision
	for range bound {
		out = append(out, CalibrationDecision{Unit: unit, Class: class, Bound: true})
	}
	for range excluded {
		out = append(out, CalibrationDecision{Unit: unit, Class: class, Bound: true, Excluded: true})
	}
	return out
}

func TestStratifyDecisionsByUnit(t *testing.T) {
	t.Parallel()

	decs := []CalibrationDecision{
		{Unit: "c1", Class: agent.ScopeClassExplicit, Bound: true},
		{Unit: "c1", Class: agent.ScopeClassExplicit, Bound: true, Excluded: true},
		{Unit: "c2", Class: agent.ScopeClassUnresolved, Bound: false},
		{Unit: "c3", Class: agent.ScopeClassExplicit, Bound: true, Pool: "resolved"},
		{Unit: "", Class: agent.ScopeClassReferentKind, Bound: true},
	}
	units := StratifyDecisionsByUnit(decs)
	require.Len(t, units, 3, "one draw per unit name; non-open pools drop out")
	require.Equal(t, "c1", units[0].ID)
	require.Len(t, units[0].Decisions, 2, "in-unit decisions keep render order")
	require.Equal(t, "c2", units[1].ID)
	require.Equal(t, "", units[2].ID, "the empty unit name is a single collapsed draw")
	require.Len(t, decs, 5, "stratification does not mutate its input")
}

func TestCalibrateOpenRenderLimit_FixedSequenceAscending(t *testing.T) {
	t.Parallel()

	// Six zero-harm units certify at alpha=0.5 (e·0.5^6≈0.043<0.05)
	// — enough to pin the walk's shape without a 78-draw fixture.
	var decs []CalibrationDecision
	for i := range 6 {
		decs = append(decs, boundDec(fmt.Sprintf("c%d", i), agent.ScopeClassExplicit, 1, 0)...)
	}
	// Grid order is normalized: the walk tests smallest cap first —
	// the conservative direction for a render limit.
	cert, err := CalibrateOpenRenderLimit(decs, []int{4, 2, 1}, 0.5, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedValue)
	require.Equal(t, 4, *cert.CertifiedValue, "clean evidence certifies the least conservative value")
	require.Equal(t, CalibrateParamOpenRenderLimit, cert.Param)
	require.Len(t, cert.Classes, 1)
	require.Equal(t, []int{1, 2, 4},
		[]int{cert.Classes[0].Candidates[0].Value, cert.Classes[0].Candidates[1].Value, cert.Classes[0].Candidates[2].Value})
}

func TestCalibrateOpenRenderLimit_NoCertificationStaysHonest(t *testing.T) {
	t.Parallel()

	// Every unit's only bound row is corpus-excluded — the harm sits
	// at render rank 0, inside every candidate's cap, so nothing can
	// certify and the certificate says so without an overlay.
	var decs []CalibrationDecision
	for i := range 8 {
		decs = append(decs, boundDec(fmt.Sprintf("c%d", i), agent.ScopeClassUnresolved, 0, 1)...)
	}
	cert, err := CalibrateOpenRenderLimit(decs, []int{1, 2, 3}, 0.5, 0.05)
	require.NoError(t, err)
	require.Nil(t, cert.CertifiedValue)
	require.Nil(t, cert.Overlay)
	require.Empty(t, cert.ParamVersion)
	require.Empty(t, cert.Guarantee, "a null run claims no bound")
	require.Len(t, cert.Classes, 1)
	require.Len(t, cert.Classes[0].Candidates, 1, "the sequence stops at the first failure")
	require.Equal(t, 8, cert.Classes[0].Candidates[0].Harms)
}

func TestCalibrateOpenRenderLimit_ClassSeparation(t *testing.T) {
	t.Parallel()

	// explicit_scope is clean; unresolved's only bound rows are
	// excluded — the veto sits at render rank 0 inside every cap.
	// The failing class must not poison the certified one — and must
	// be named in the caveats.
	var decs []CalibrationDecision
	for i := range 6 {
		decs = append(decs, boundDec(fmt.Sprintf("e%d", i), agent.ScopeClassExplicit, 1, 0)...)
	}
	for i := range 8 {
		decs = append(decs, boundDec(fmt.Sprintf("u%d", i), agent.ScopeClassUnresolved, 0, 1)...)
	}
	cert, err := CalibrateOpenRenderLimit(decs, []int{1, 2, 3}, 0.5, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedValue)
	require.Equal(t, 3, *cert.CertifiedValue)
	require.Len(t, cert.Classes, 2, "only classes with evidence emit runs")
	require.Equal(t, agent.ScopeClassExplicit, cert.Classes[0].Class)
	require.Equal(t, 3, *cert.Classes[0].CertifiedValue)
	require.Equal(t, agent.ScopeClassUnresolved, cert.Classes[1].Class)
	require.Nil(t, cert.Classes[1].CertifiedValue)
	require.Contains(t, cert.Guarantee, agent.ScopeClassExplicit)
	require.NotContains(t, cert.Guarantee, agent.ScopeClassUnresolved)
	var joined string
	for _, c := range cert.Caveats {
		joined += c + "\n"
	}
	require.Contains(t, joined, "could not certify")
	require.Contains(t, joined, agent.ScopeClassUnresolved)
	require.Contains(t, joined, "calibration distribution", "the guarantee never claims the selector is safe")
}

func TestCalibrateOpenRenderLimit_ComposedValueIsMinAcrossClasses(t *testing.T) {
	t.Parallel()

	// explicit certifies the whole grid; referent_kind carries one
	// unit whose excluded row only renders at cap>=3 — it certifies
	// 1 and 2, fails 3, and the composed overlay value is the min.
	var decs []CalibrationDecision
	for i := range 6 {
		decs = append(decs, boundDec(fmt.Sprintf("e%d", i), agent.ScopeClassExplicit, 1, 0)...)
	}
	for i := range 6 {
		decs = append(decs, boundDec(fmt.Sprintf("k%d", i), agent.ScopeClassReferentKind, 1, 0)...)
	}
	// The harm unit: two clean bound rows rank ahead of the
	// excluded one — k=1,2 keep it out of the render, k>=3 lets it
	// through.
	decs = append(decs,
		CalibrationDecision{Unit: "kh", Class: agent.ScopeClassReferentKind, Bound: true},
		CalibrationDecision{Unit: "kh", Class: agent.ScopeClassReferentKind, Bound: true},
		CalibrationDecision{Unit: "kh", Class: agent.ScopeClassReferentKind, Bound: true, Excluded: true},
	)
	cert, err := CalibrateOpenRenderLimit(decs, []int{1, 2, 3, 4}, 0.5, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.CertifiedValue)
	require.Equal(t, 2, *cert.CertifiedValue,
		"the deployed cap is one value — it must hold under every certified class")
	require.Equal(t, 4, *cert.Classes[0].CertifiedValue)
	require.Equal(t, 2, *cert.Classes[1].CertifiedValue)
	require.Len(t, cert.Classes[1].Candidates, 3, "referent_kind stops at the first failure")
	require.Equal(t, 7, cert.Classes[1].Candidates[2].Admits)
	require.Equal(t, 1, cert.Classes[1].Candidates[2].Harms)
}

func TestCalibrateOpenRenderLimit_UnitIsTheDraw(t *testing.T) {
	t.Parallel()

	// A unit's harm is any-violation, not per-row: two excluded rows
	// in one unit count once, and the admit population counts units
	// — six units here, even though the first carries three
	// decisions.
	var decs []CalibrationDecision
	decs = append(decs,
		CalibrationDecision{Unit: "c0", Class: agent.ScopeClassExplicit, Bound: true, Excluded: true},
		CalibrationDecision{Unit: "c0", Class: agent.ScopeClassExplicit, Bound: true, Excluded: true},
		CalibrationDecision{Unit: "c0", Class: agent.ScopeClassExplicit, Bound: false, Excluded: true},
	)
	for i := range 5 {
		decs = append(decs, boundDec(fmt.Sprintf("c%d", i+1), agent.ScopeClassExplicit, 1, 0)...)
	}
	cert, err := CalibrateOpenRenderLimit(decs, []int{3}, 0.5, 0.05)
	require.NoError(t, err)
	require.Len(t, cert.Classes, 1)
	require.Equal(t, 6, cert.Classes[0].Draws, "the cell, not the decision, is the draw")
	require.Equal(t, 6, cert.Classes[0].Candidates[0].Admits)
	require.Equal(t, 1, cert.Classes[0].Candidates[0].Harms,
		"several excluded rows in one unit are one violation")
}

func TestCalibrateOpenRenderLimit_OverlayRoundTripsThroughParams(t *testing.T) {
	t.Parallel()

	var decs []CalibrationDecision
	for i := range 6 {
		decs = append(decs, boundDec(fmt.Sprintf("c%d", i), agent.ScopeClassExplicit, 1, 0)...)
	}
	cert, err := CalibrateOpenRenderLimit(decs, []int{2}, 0.5, 0.05)
	require.NoError(t, err)
	require.NotNil(t, cert.Overlay)

	// The certificate's overlay must survive the production apply
	// path — unknown fields, type checks, and bounds all enforced
	// by ResolveMemory — and reproduce the stamped param_version.
	m, err := params.ResolveMemory(cert.Overlay)
	require.NoError(t, err)
	require.Equal(t, *cert.CertifiedValue, m.OpenRenderLimit)
	require.Equal(t, cert.ParamVersion, m.Version())
	require.Equal(t, 2, m.OpenRenderLimit)
	require.Equal(t, params.DefaultMemory().ResolvedRenderLimit, m.ResolvedRenderLimit,
		"the overlay touches only the certified knob")
}

func TestCalibrateOpenRenderLimit_Validation(t *testing.T) {
	t.Parallel()

	_, err := CalibrateOpenRenderLimit(nil, nil, 0.5, 0.05)
	require.Error(t, err)
	_, err = CalibrateOpenRenderLimit(nil, []int{1}, 0, 0.05)
	require.Error(t, err)
	_, err = CalibrateOpenRenderLimit(nil, []int{1}, 0.5, 1.5)
	require.Error(t, err)
}

func TestProjectBindingDecisions_RealCorpus(t *testing.T) {
	t.Parallel()

	// The committed labeled corpus is the end-to-end pin: every
	// cell projects its candidates, the class vocabulary covers the
	// corpus, and the projected bound bit reproduces the cell's
	// labeled admit verdict — a signature-keying or pool-mixing bug
	// breaks exactly one of these.
	decs, err := LoadBindingDecisions("../agent/testdata/failure_binding.jsonl", t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, decs)

	classes := map[string]int{}
	units := map[string]int{}
	for _, d := range decs {
		require.NotEmpty(t, d.Unit)
		units[d.Unit]++
		classes[d.Class]++
	}
	require.Len(t, units, 55, "one draw per corpus cell")
	require.Greater(t, classes[agent.ScopeClassExplicit], 0)
	require.Greater(t, classes[agent.ScopeClassReferentKind]+classes[agent.ScopeClassUnresolved], 0)

	var bound, excluded int
	for _, d := range decs {
		if d.Bound {
			bound++
		}
		if d.Excluded {
			excluded++
		}
	}
	require.Equal(t, 28, bound, "corpus admits: the selector still agrees with every labeled admit")
	require.Equal(t, 32, excluded, "the labeled rejections all project")
}
