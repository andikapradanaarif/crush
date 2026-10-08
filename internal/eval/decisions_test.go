package eval

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// The decision corpus is the gate's calibration set — preserved and
// synthetic record files under testdata/decisions/, one directory
// per named failure class. Each scenario is a mini eval dir:
//
//	testdata/decisions/<name>/
//	  experiment.json                      the manifest under test
//	  expect.json                          the asserted verdict
//	  results/<exp>/runs.jsonl             RunRecord lines
//	  results/<exp>/report-<inv>.json      the run-time alarm snapshot
//
// The milestone's list is the spec: noisy-null, known pass
// regression, lower-calls-worse-pass, cost inflation, missing
// telemetry, valid abstention — plus the #197 acceptance classes
// (floor violations, within-band cost, real improvement inside the
// allowance) and sidecar-spend attribution. New failure classes land
// as data, not new Go.
type decisionExpect struct {
	// Note is the scenario's calibration intent — printed on failure.
	Note         string `json:"note"`
	CompareError string `json:"compare_error,omitempty"`
	// RuleResult asserts CompareReport.RuleResult exactly:
	// "satisfied", "not satisfied", "inconclusive", "absent".
	RuleResult         string   `json:"rule_result,omitempty"`
	RuleNoteContains   string   `json:"rule_note_contains,omitempty"`
	PrimaryVerdict     string   `json:"primary_verdict_contains,omitempty"`
	AcceptanceContains string   `json:"acceptance_contains,omitempty"`
	AlarmsContain      []string `json:"alarms_contain,omitempty"`
	SummaryContains    []string `json:"summary_contains,omitempty"`
}

func TestDecisionCorpus(t *testing.T) {
	t.Parallel()
	root := filepath.Join("testdata", "decisions")
	dirs, err := filepath.Glob(filepath.Join(root, "*"))
	require.NoError(t, err)
	require.NotEmpty(t, dirs, "decision corpus is empty — every calibration class needs a fixture dir")
	for _, dir := range dirs {
		dir := dir
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expPath := filepath.Join(dir, "experiment.json")
			exp, err := LoadExperiment(expPath)
			require.NoError(t, err, "%s: manifest must validate — an invalid scenario tests nothing", name)

			data, err := os.ReadFile(filepath.Join(dir, "expect.json"))
			require.NoError(t, err)
			var want decisionExpect
			require.NoError(t, json.Unmarshal(data, &want))

			rep, err := seededRunner(dir).Compare(exp, "i1")
			if want.CompareError != "" {
				require.ErrorContains(t, err, want.CompareError, "%s: %s", name, want.Note)
				return
			}
			require.NoError(t, err, "%s: %s", name, want.Note)
			if want.RuleResult != "" {
				require.Equal(t, want.RuleResult, rep.RuleResult, "%s: %s", name, want.Note)
			}
			if want.RuleNoteContains != "" {
				require.Contains(t, rep.RuleNote, want.RuleNoteContains, "%s", name)
			}
			if want.PrimaryVerdict != "" {
				require.NotEmpty(t, rep.Metrics)
				require.Contains(t, rep.Metrics[0].Verdict, want.PrimaryVerdict, "%s", name)
			}
			if want.AcceptanceContains != "" {
				require.NotEmpty(t, rep.Metrics)
				require.Contains(t, rep.Metrics[0].Acceptance, want.AcceptanceContains, "%s", name)
			}
			for _, a := range want.AlarmsContain {
				require.Contains(t, fmt.Sprint(rep.AcceptanceAlarms), a, "%s", name)
			}
			summary := rep.Summary()
			for _, s := range want.SummaryContains {
				require.Contains(t, summary, s, "%s", name)
			}
		})
	}
}

// TestDecisionCorpus_Generate rewrites every fixture under
// testdata/decisions/ — records, snapshots, manifests, expectations.
// It exists so the corpus is reproducible by construction rather than
// hand-tooled; run it with EVAL_GEN=1. Gated off by default so the
// committed files — not the generator — are the reviewed artifact.
func TestDecisionCorpus_Generate(t *testing.T) {
	if os.Getenv("EVAL_GEN") != "1" {
		t.Skip("set EVAL_GEN=1 to regenerate the decision corpus")
	}
	for _, sc := range decisionScenarios() {
		dir := filepath.Join("testdata", "decisions", sc.name)
		writeScenario(t, dir, sc)
		t.Logf("wrote %s", dir)
	}
}

// decisionScenario is one corpus cell's spec — the generator reads
// this, the driver asserts the expectation it writes.
type decisionScenario struct {
	name     string
	exp      *Experiment
	snapshot *alarmSnapshot // nil → no snapshot persisted (legacy path)
	recs     []RunRecord
	expect   decisionExpect
}

// writeScenario materializes one scenario dir in eval-dir layout:
// the manifest and expectation at top level, the invocation's records
// and alarm snapshot under results/<exp>/ exactly where Compare's
// loader walks.
func writeScenario(t *testing.T, dir string, sc decisionScenario) {
	t.Helper()
	require.NoError(t, ValidateExperiment(sc.exp)) // Validate first: the snapshot carries the normalized rule.
	resDir := filepath.Join(dir, "results", sc.exp.Name)
	require.NoError(t, os.MkdirAll(resDir, 0o755))

	expJSON, err := json.MarshalIndent(sc.exp, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "experiment.json"), expJSON, 0o644))

	wantJSON, err := json.MarshalIndent(sc.expect, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "expect.json"), wantJSON, 0o644))

	var lines []byte
	for _, rec := range sc.recs {
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		lines = append(lines, append(data, '\n')...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(resDir, "runs.jsonl"), lines, 0o644))

	if sc.snapshot != nil {
		snapJSON, err := json.MarshalIndent(sc.snapshot, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(resDir, "report-i1.json"), snapJSON, 0o644))
	}
}

// --- Fixture builders -------------------------------------------------------

var (
	decOptsC = map[string]any{"memory": "off"}
	decOptsT = map[string]any{"memory": "on"}
)

// decRec builds one run record for the corpus. outcome defaults to
// pass; gen is the sidecar spend (nil = none emitted).
func decRec(exp, traj, arm, inv string, idx, steps, input, output int, gen *GeneratorTokens) RunRecord {
	res := decOptsC
	if arm == ArmTreatment {
		res = decOptsT
	}
	return RunRecord{
		Experiment:      exp,
		TrajectoryID:    traj,
		Arm:             arm,
		Invocation:      inv,
		RunIndex:        idx,
		Outcome:         OutcomePass,
		Steps:           steps,
		Tokens:          TokenUsage{Input: int64(input), Output: int64(output)},
		GeneratorTokens: gen,
		ResolvedOptions: res,
	}
}

// decPairs builds n matched pairs on one trajectory — control cost
// cIn/cSteps, treatment tIn/tSteps. Variation is baked in so the CI
// has honest width rather than degenerating to a point.
func decPairs(exp, inv string, n int, cSteps, tSteps, cIn, tIn int) []RunRecord {
	var recs []RunRecord
	for i := range n {
		ci := cIn + i*7
		ti := tIn + i*7
		recs = append(recs,
			decRec(exp, "t1", ArmControl, inv, i, cSteps, ci, 100, nil),
			decRec(exp, "t1", ArmTreatment, inv, i, tSteps, ti, 100, nil),
		)
	}
	return recs
}

// decSnap is the snapshot a completed invocation persists — the
// normalized declarations, gate pass, no alarms.
func decSnap(exp *Experiment) *alarmSnapshot {
	return &alarmSnapshot{
		Invocation:   "i1",
		GateVerdict:  "pass",
		Primary:      exp.Primary,
		DecisionRule: exp.DecisionRule,
	}
}

func rulePrimary(metric, dir string, imp float64) *RulePrimary {
	return &RulePrimary{Metric: metric, Arm: ArmTreatment, Vs: ArmControl, Direction: dir, MinImprovement: imp}
}

func decExperiment(rule *DecisionRule, weights *CostWeights) *Experiment {
	return &Experiment{
		Name:              "dec",
		Model:             "mock/model",
		Temperature:       ptr(0.0),
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandStable: 4},
		Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
		CostWeights:       weights,
		DecisionRule:      rule,
	}
}

// decisionScenarios is the corpus table — each entry one calibration
// cell. Ordering is the milestone's failure-class list.
func decisionScenarios() []decisionScenario {
	out := []decisionScenario{}

	// noisy-null: identical distributions with real spread. The CI
	// spans the declared bound — the rule must not call it either
	// way, and nothing may read as an effect.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 15%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.15),
		}, nil)
		exp.Name = "dec-noisy-null"
		var recs []RunRecord
		for i := range 8 {
			treat := 500
			if i%2 == 0 {
				treat = 1500
			}
			recs = append(recs,
				decRec(exp.Name, "t1", ArmControl, "i1", i, 10, 1000, 100, nil),
				decRec(exp.Name, "t1", ArmTreatment, "i1", i, 10, treat, 100, nil))
		}
		out = append(out, decisionScenario{
			name: "noisy-null", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:             "A noisy null must not satisfy a declared improvement — the CI spanning the bound is 'inconclusive', and nothing reads as an effect.",
				RuleResult:       "inconclusive",
				RuleNoteContains: "CI spans",
			},
		})
	}

	// known-regression: treatment is conclusively +30% on the
	// claimed-decrease metric — the data disproves the hypothesis.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 15%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.15),
		}, nil)
		exp.Name = "dec-known-regression"
		recs := decPairs(exp.Name, "i1", 8, 10, 10, 1000, 1330)
		out = append(out, decisionScenario{
			name: "known-regression", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:       "A real regression against the claim is 'not satisfied' — disproof is a different verdict from indecision.",
				RuleResult: "not satisfied",
			},
		})
	}

	// lower-calls-worse-pass: the arm is cheaper but fails more —
	// the pass_rate guardrail falsifies the claim even though the
	// primary clears.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens 15% at no worse pass rate",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.15),
			Guardrail:  &RuleGuardrail{Metric: "pass_rate", MinDelta: 0},
		}, nil)
		exp.Name = "dec-lower-calls-worse-pass"
		recs := decPairs(exp.Name, "i1", 8, 8, 10, 700, 1000)
		for _, idx := range []int{5, 7} { // Two conclusive fails: 75% vs 100%.
			for i := range recs {
				if recs[i].Arm == ArmTreatment && recs[i].RunIndex == idx {
					recs[i].Outcome = OutcomeFail
				}
			}
		}
		out = append(out, decisionScenario{
			name: "lower-calls-worse-pass", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:       "Cheaper-but-flakier is not better — the guardrail's conclusive failure falsifies the rule even with the primary cleared.",
				RuleResult: "not satisfied",
			},
		})
	}

	// cost-inflation-in-band: ΔS inside the noise band, ΔC growing —
	// the #197 rule refuses the spend and the declared rule fails.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 10%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.10),
		}, &CostWeights{CacheRead: 0.1, CacheWrite: 0.25, Output: 4})
		exp.Name = "dec-cost-inflation"
		exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.15, Floor: ptr(0.5)}
		recs := decPairs(exp.Name, "i1", 8, 10, 9, 1000, 1400) // steps −10% (in δ), cost +40%.
		out = append(out, decisionScenario{
			name: "cost-inflation-in-band", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:               "Within-band quality movement cannot justify token growth — noise-level effects may only ship cost-neutral.",
				RuleResult:         "not satisfied",
				AcceptanceContains: "cost-unjustified",
				AlarmsContain:      []string{"cost-unjustified"},
			},
		})
	}

	// missing-telemetry: the claimed metric's source never produced
	// data — absent is unmeasurable, never a silent zero.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts map calls by 10%",
			Primary:    rulePrimary("call_metrics.map_calls", PrimaryDecrease, 0.10),
		}, nil)
		exp.Name = "dec-missing-telemetry"
		recs := decPairs(exp.Name, "i1", 8, 10, 10, 1000, 900)
		// No record carries call_metrics — the treatment that would
		// emit it isn't in this corpus's wiring.
		out = append(out, decisionScenario{
			name: "missing-telemetry", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:             "A metric whose source never emitted is unmeasurable — the rule is inconclusive, not zero.",
				RuleResult:       "inconclusive",
				RuleNoteContains: "no measurable pairs",
			},
		})
	}

	// valid-abstention: the declared exclusion explains the
	// treatment shortfall — the invocation stays evaluable and the
	// remaining conclusive pairs still answer the rule.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 10%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.10),
		}, nil)
		exp.Name = "dec-valid-abstention"
		exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmTreatment, ErrorClass: "window_cap_enforced", Min: 2}
		recs := decPairs(exp.Name, "i1", 6, 10, 10, 1000, 780)
		// Two designed deaths — the excluded class the declaration
		// names — then the conclusive pairs that remain decide.
		for i := 6; i < 8; i++ {
			r := decRec(exp.Name, "t1", ArmTreatment, "i1", i, 10, 780, 100, nil)
			r.Outcome = OutcomeError
			r.ErrorClass = "window_cap_enforced"
			recs = append(recs, r)
		}
		snap := decSnap(exp)
		snap.OutcomeAlarms = []string{"expected-exclusion met: t1 (2 window_cap_enforced)"}
		out = append(out, decisionScenario{
			name: "valid-abstention", exp: exp, snapshot: snap, recs: recs,
			expect: decisionExpect{
				Note:       "A declared exclusion is data — the regime engaged, the shortfall is explained, and the surviving pairs still decide the claim.",
				RuleResult: "satisfied",
			},
		})
	}

	// floor-violated-treatment: the rule's claim clears but the
	// quality floor doesn't — cost savings on a regressing arm are
	// not savings.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 10%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.10),
		}, &CostWeights{CacheRead: 0.1, CacheWrite: 0.25, Output: 4})
		exp.Name = "dec-floor-violated-treatment"
		exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.15, Floor: ptr(0.95)}
		recs := decPairs(exp.Name, "i1", 8, 10, 7, 1000, 780)
		// One conclusive fail in 8 → 87.5% < the 95% floor.
		for i := range recs {
			if recs[i].Arm == ArmTreatment && recs[i].RunIndex == 3 {
				recs[i].Outcome = OutcomeFail
			}
		}
		out = append(out, decisionScenario{
			name: "floor-violated-treatment", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:               "A pass rate under the floor fails acceptance regardless of cost — the claim clearing is not the same as the arm being shippable.",
				RuleResult:         "satisfied",
				PrimaryVerdict:     "FLOOR VIOLATED",
				AcceptanceContains: "floor-violated",
			},
		})
	}

	// floor-violated-control: the same bound on the baseline — a weak
	// control is a corpus miss, not evidence.
	{
		exp := decExperiment(nil, &CostWeights{CacheRead: 0.1, CacheWrite: 0.25, Output: 4})
		exp.Name = "dec-floor-violated-control"
		exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.15, Floor: ptr(0.95)}
		recs := decPairs(exp.Name, "i1", 8, 10, 7, 1000, 780)
		for i := range recs {
			if recs[i].Arm == ArmControl && recs[i].RunIndex == 2 {
				recs[i].Outcome = OutcomeFail
			}
		}
		out = append(out, decisionScenario{
			name: "floor-violated-control", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:               "A control under the floor means the corpus never produced the regime — reported as the baseline breach, not as treatment failure.",
				AcceptanceContains: "floor-violated",
				AlarmsContain:      []string{"floor-violated"},
				RuleResult:         "absent",
			},
		})
	}

	// real-improvement-in-allowance: ΔS beyond δ carrying bounded
	// cost growth — the acceptance leg passes and the declared rule
	// is satisfied.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts steps by 20%",
			Primary:    rulePrimary("steps", PrimaryDecrease, 0.20),
		}, &CostWeights{CacheRead: 0.1, CacheWrite: 0.25, Output: 4})
		exp.Name = "dec-real-improvement"
		exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.15, Floor: ptr(0.9)}
		recs := decPairs(exp.Name, "i1", 8, 10, 7, 1000, 1080) // steps −30%; cost +~7% ≤ β₀.
		out = append(out, decisionScenario{
			name: "real-improvement-in-allowance", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:               "A real effect may carry flat bounded cost growth — this is the rule's green path, not a loophole.",
				RuleResult:         "satisfied",
				AcceptanceContains: "ok",
			},
		})
	}

	// generator-cost-attribution: main-model tokens drop while the
	// sidecar quietly carries the spend — tokens_to_done prices both
	// tiers, so the claim fails instead of laundering cost.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory reduces cost per pass",
			Primary:    rulePrimary("tokens_to_done", PrimaryDecrease, 0.0),
		}, &CostWeights{CacheRead: 0.1, CacheWrite: 0.25, Output: 4, Generator: &CostWeights{Output: 1}})
		exp.Name = "dec-generator-attribution"
		var recs []RunRecord
		for i := range 8 {
			recs = append(recs,
				decRec(exp.Name, "t1", ArmControl, "i1", i, 10, 1000+i*7, 100, nil),
				decRec(exp.Name, "t1", ArmTreatment, "i1", i, 8, 600+i*7, 100,
					&GeneratorTokens{Calls: 3, Input: 800, Output: 50}))
		}
		out = append(out, decisionScenario{
			name: "generator-cost-attribution", exp: exp, snapshot: decSnap(exp), recs: recs,
			expect: decisionExpect{
				Note:       "Spend outsourced to the sidecar still prices — main tokens −40% but generator spend makes cost-per-pass +~40%: the claim is falsified, not hidden.",
				RuleResult: "not satisfied",
			},
		})
	}

	// post-hoc rule: the invocation ran uncommitted and the manifest
	// grew a rule afterward — provenance suppresses it.
	{
		exp := decExperiment(&DecisionRule{
			Hypothesis: "memory cuts input tokens by 15%",
			Primary:    rulePrimary("tokens.input", PrimaryDecrease, 0.15),
		}, nil)
		exp.Name = "dec-post-hoc"
		snap := &alarmSnapshot{Invocation: "i1", GateVerdict: "pass"} // No rule pinned.
		recs := decPairs(exp.Name, "i1", 8, 10, 10, 1000, 750)
		out = append(out, decisionScenario{
			name: "post-hoc-rule", exp: exp, snapshot: snap, recs: recs,
			expect: decisionExpect{
				Note:             "A rule declared after the run can't be read back as pre-committed — suppressed to inconclusive.",
				RuleResult:       "inconclusive",
				RuleNoteContains: "post-hoc",
			},
		})
	}

	return out
}

// Scenario-name sanity: the corpus dirs and the scenario table must
// agree — a stale dir without a generator entry still runs (the
// corpus is data), and a generator entry without a dir means
// EVAL_GEN=1 hasn't run since the scenario was added.
func TestDecisionCorpus_DirParity(t *testing.T) {
	if os.Getenv("EVAL_GEN") == "1" {
		t.Skip("generation mode")
	}
	t.Parallel()
	dirs, err := filepath.Glob(filepath.Join("testdata", "decisions", "*"))
	require.NoError(t, err)
	have := map[string]bool{}
	for _, d := range dirs {
		have[filepath.Base(d)] = true
	}
	for _, sc := range decisionScenarios() {
		require.True(t, have[sc.name], "scenario %q has no fixture dir — run EVAL_GEN=1", sc.name)
		delete(have, sc.name)
	}
	require.Empty(t, have, "fixture dirs without generator entries: %v — the corpus should be regenerable", slices.Sorted(maps.Keys(have)))
}
