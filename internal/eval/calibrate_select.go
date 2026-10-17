package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/params"
)

// Selector-side LTT calibration for params.Memory values (#342).
// The referent-episode corpus (#306) has no volume on real projects;
// the labeled selector corpora already supply decisions with
// known-correct outcomes — the binding benchmark's per-candidate
// veto labels. This file mirrors CalibratePromoteHits: one
// fixed-sequence most-conservative→least walk per ambiguity class
// (exchangeability is per class — a prompt's scope regime decides
// what can bind), a SelectorCertificate composing the class runs,
// and a versioned overlay only when a class certifies.

const (
	// CalibrateParamOpenRenderLimit is the open_render_limit knob —
	// the open-pool render cap, the shipped surface a certified
	// value lands on. The corpus's veto labels score its harm: a
	// bound row the cell labels excluded is a wrong render the cap
	// either contained or did not. The alternative the issue names,
	// the resolver's acceptance rule, is not a memory_params value
	// and has no labeled outcome the corpus isolates — the limit is
	// the knob the evidence can actually certify.
	CalibrateParamOpenRenderLimit = "open_render_limit"
	// CalibrateMetricVetoRender is the unit-level harm: the share
	// of stratified units whose capped render contains at least
	// one row the corpus labels excluded. Per-unit any-violation,
	// not the row-level veto rate the benchmark reports — a tail
	// either carried a wrong row or it did not, and the cell-level
	// question is what deployment actually asks.
	CalibrateMetricVetoRender = "veto_violating_render"
)

// CalibrationDecision is one selector verdict in calibration form —
// the eval-local projection of a FailureDecision plus the corpus's
// known-correct label, so the LTT core is testable without a
// selector or a corpus on disk. Producers emit a unit's decisions in
// render order: the render cap keeps the leading bound rows, so the
// calibrator computes a decision's bound rank from its position.
type CalibrationDecision struct {
	// Unit is the independence unit the exchangeability contract
	// stratifies on — a corpus cell or a recorded run. Decisions in
	// one unit share the prompt and candidate state, so they are
	// one draw, not several.
	Unit string
	// Class is the prompt's deterministic scope class — one of
	// agent.ScopeClassExplicit, agent.ScopeClassReferentKind,
	// agent.ScopeClassUnresolved. Harm concentrates differently per
	// regime, so classes calibrate separately. It must be constant
	// within a unit — the calibrator rejects a mixed-class stream.
	Class string
	// Pool is the memory pool the decision came from — "open",
	// "resolved", or "command"; empty reads as "open" (the wire
	// default). open_render_limit governs the open pool only.
	Pool string
	// Bound reports the selector bound the row — it renders under
	// an unlimited cap. render_capped is a budget verdict, not a
	// binding one, so producers record uncapped decisions — or, on
	// a live-telemetry projection (#341 tail.decisions), the
	// mapping is Bound = Admit || Reason == "render_capped": a
	// naive Bound = Admit silently drops the capped tail, leaving
	// candidate caps larger than the deployed one unmodelable.
	Bound bool
	// Excluded is the corpus's known-correct label: the row should
	// NOT have rendered (the cell expected a rejection). A bound
	// excluded row that makes the cap is the harm this run bounds.
	Excluded bool
}

// DecisionUnit is one stratified draw — a corpus cell or recorded
// run — with its open-pool decisions in render order. Class applies
// to the whole unit: it is a prompt property.
type DecisionUnit struct {
	ID        string
	Class     string
	Decisions []CalibrationDecision
}

// boundRank returns the decision's position among its unit's bound
// rows — render order — or -1 when unbound.
func (u DecisionUnit) boundRank(i int) int {
	if !u.Decisions[i].Bound {
		return -1
	}
	rank := 0
	for j := 0; j < i; j++ {
		if u.Decisions[j].Bound {
			rank++
		}
	}
	return rank
}

// rendersAt reports whether the unit has any bound row rendering
// under cap k — the unit's admit event for that candidate.
func (u DecisionUnit) rendersAt(k int) bool {
	for i := range u.Decisions {
		if r := u.boundRank(i); r >= 0 && r < k {
			return true
		}
	}
	return false
}

// harmAt reports whether the unit's k-capped render contains a row
// the corpus labels excluded — the veto harm for that candidate.
func (u DecisionUnit) harmAt(k int) bool {
	for i := range u.Decisions {
		if u.Decisions[i].Bound && u.Decisions[i].Excluded {
			if r := u.boundRank(i); r >= 0 && r < k {
				return true
			}
		}
	}
	return false
}

// StratifyDecisionsByUnit groups a decision stream into its
// exchangeability units — cells or runs, never sessions: the
// corpus's independence contract is the authored instance, not the
// project session that produced it. Units keep first-appearance
// order and decisions keep their in-unit render order; the input is
// not mutated. A unit's class is taken from its first decision —
// producers emit one class per unit (the calibrator rejects mixed
// streams upstream). The empty unit name collapses into a single
// draw — an unnamed stream is one correlated context by
// construction.
func StratifyDecisionsByUnit(decs []CalibrationDecision) []DecisionUnit {
	idx := map[string]int{}
	var out []DecisionUnit
	for _, d := range decs {
		// open_render_limit governs the open pool; knowledge pools
		// carry their own caps and corpus labels do not cover them.
		if d.Pool != "" && d.Pool != "open" {
			continue
		}
		i, ok := idx[d.Unit]
		if !ok {
			i = len(out)
			idx[d.Unit] = i
			out = append(out, DecisionUnit{ID: d.Unit, Class: d.Class})
		}
		out[i].Decisions = append(out[i].Decisions, d)
	}
	return out
}

// ClassCandidate records one grid point's test outcome inside one
// class run — the selector analogue of CandidateResult without the
// suggested-admit split (referent evidence machinery that has no
// selector counterpart).
type ClassCandidate struct {
	Value     int     `json:"value"`
	Admits    int     `json:"admits"`
	Harms     int     `json:"harms"`
	RiskHat   float64 `json:"risk_hat"`
	PValue    float64 `json:"p_value"`
	Certified bool    `json:"certified"`
}

// ClassCertificate is one ambiguity class's fixed-sequence result —
// its own draws, candidates, and certified value or honest absence.
// Delta is the level this class's walk ran at — the certificate's
// family-wise budget split evenly across the classes that had
// draws, so the classes' certified bounds hold jointly at the
// certificate's delta.
type ClassCertificate struct {
	Class          string           `json:"class"`
	Delta          float64          `json:"delta"`
	Draws          int              `json:"draws"`
	Candidates     []ClassCandidate `json:"candidates"`
	CertifiedValue *int             `json:"certified_value"`
}

// SelectorCertificate composes the per-class LTT runs for one
// selector knob (#342). Classes carry each run's certificate; the
// composed CertifiedValue — when any class certified — is the most
// conservative certified cap across classes, because the deployed
// open_render_limit is one global value whose bound must hold under
// every certified class's distribution. The class walks split the
// family-wise budget evenly (each runs at delta/m for m classes
// with draws), so the joint claim over certified classes holds at
// the certificate's delta — not m·delta. Decisions is the input
// stream's size before pool filtering; the per-class draw counts
// are the stratification's. Guarantee and Caveats name the corpus
// distribution explicitly: the run bounds harm on the labeled
// decisions it saw, never "the selector is safe".
type SelectorCertificate struct {
	Param          string             `json:"param"`
	Metric         string             `json:"metric"`
	Alpha          float64            `json:"alpha"`
	Delta          float64            `json:"delta"`
	Decisions      int                `json:"decisions"`
	Classes        []ClassCertificate `json:"classes"`
	CertifiedValue *int               `json:"certified_value"`
	Overlay        map[string]any     `json:"overlay,omitempty"`
	// OverlayBase names the param set the stamped version assumes —
	// applying the overlay onto a non-default memory_params resolves
	// to a different param_version than stamped here.
	OverlayBase  string   `json:"overlay_base,omitempty"`
	ParamVersion string   `json:"param_version,omitempty"`
	Guarantee    string   `json:"guarantee,omitempty"`
	Caveats      []string `json:"caveats"`
}

// selectorClassOrder is the deterministic emission order for the
// closed class vocabulary — the three regimes the issue names
// first, then any producer-invented class appended in
// first-appearance order, so the output is stable for a given
// input.
var selectorClassOrder = []string{
	agent.ScopeClassExplicit,
	agent.ScopeClassReferentKind,
	agent.ScopeClassUnresolved,
}

// CalibrateOpenRenderLimit runs the per-class fixed-sequence LTT
// test for OpenRenderLimit (#342). grid must list candidate caps
// most-conservative → least — ascending: a smaller cap renders a
// subset of every larger cap's bound rows, so tightening is the
// conservative direction. Each class's walk stops at the first
// value it cannot certify; the composed certified value is the
// smallest certified cap across classes — every class's bound
// holds at it (the walk certified every smaller tested value).
// The classes split the family-wise budget evenly (each walk runs
// at delta/m for m classes with draws), so the certificate's joint
// claim over certified classes holds at delta. A class with no
// evidence cannot certify and does not poison the others; the
// caveat names it.
func CalibrateOpenRenderLimit(decs []CalibrationDecision, grid []int, alpha, delta float64) (SelectorCertificate, error) {
	if len(grid) == 0 {
		return SelectorCertificate{}, fmt.Errorf("empty candidate grid")
	}
	if alpha <= 0 || alpha >= 1 || delta <= 0 || delta >= 1 {
		return SelectorCertificate{}, fmt.Errorf("alpha and delta must be in (0,1): alpha=%v delta=%v", alpha, delta)
	}
	grid = slices.Clone(grid)
	slices.Sort(grid)
	grid = slices.Compact(grid)
	if grid[0] <= 0 {
		// A non-positive cap renders nothing — admits=0 gives p=1,
		// which would halt the walk at the first candidate and
		// mislabel the class as unevidenced. Channel-off is a
		// deployment state, not a certifiable value.
		return SelectorCertificate{}, fmt.Errorf("grid values must be positive: %v", grid[0])
	}

	// A unit's class is a prompt property — one value per unit. A
	// mixed-class stream is a producer bug; merging it would drop
	// one class's harm into another's run, so it fails closed.
	unitClass := map[string]string{}
	for _, d := range decs {
		if prev, ok := unitClass[d.Unit]; ok && prev != d.Class {
			return SelectorCertificate{}, fmt.Errorf(
				"unit %q carries mixed scope classes (%q then %q)", d.Unit, prev, d.Class)
		}
		unitClass[d.Unit] = d.Class
	}

	units := StratifyDecisionsByUnit(decs)
	byClass := map[string][]DecisionUnit{}
	classes := slices.Clone(selectorClassOrder)
	for _, u := range units {
		if _, ok := byClass[u.Class]; !ok && !slices.Contains(classes, u.Class) {
			classes = append(classes, u.Class)
		}
		byClass[u.Class] = append(byClass[u.Class], u)
	}

	// The classes' walks split the family-wise budget evenly so the
	// certificate's joint claim — every certified class's bound
	// holding at the composed value — is honest at delta rather
	// than silently degrading to m·delta.
	m := 0
	for _, class := range classes {
		if len(byClass[class]) > 0 {
			m++
		}
	}
	classDelta := delta
	if m > 0 {
		classDelta = delta / float64(m)
	}

	cert := SelectorCertificate{
		Param:     CalibrateParamOpenRenderLimit,
		Metric:    CalibrateMetricVetoRender,
		Alpha:     alpha,
		Delta:     delta,
		Decisions: len(decs),
	}
	var certifiedClasses, unevidenced, uncertified []string
	for _, class := range classes {
		draws := byClass[class]
		if len(draws) == 0 {
			continue
		}
		cc := ClassCertificate{Class: class, Delta: classDelta, Draws: len(draws)}
		for _, k := range grid {
			var res ClassCandidate
			res.Value = k
			for _, u := range draws {
				if !u.rendersAt(k) {
					continue
				}
				res.Admits++
				if u.harmAt(k) {
					res.Harms++
				}
			}
			res.PValue = hbUpperPValue(res.Harms, res.Admits, alpha)
			if res.Admits > 0 {
				res.RiskHat = float64(res.Harms) / float64(res.Admits)
			}
			res.Certified = res.PValue <= classDelta && res.Admits > 0
			cc.Candidates = append(cc.Candidates, res)
			if !res.Certified {
				break
			}
			cc.CertifiedValue = new(int)
			*cc.CertifiedValue = k
		}
		cert.Classes = append(cert.Classes, cc)
		switch {
		case cc.CertifiedValue != nil:
			certifiedClasses = append(certifiedClasses, class)
			if cert.CertifiedValue == nil || *cc.CertifiedValue < *cert.CertifiedValue {
				cert.CertifiedValue = new(int)
				*cert.CertifiedValue = *cc.CertifiedValue
			}
		case cc.Candidates[0].Admits == 0:
			unevidenced = append(unevidenced, class)
		default:
			uncertified = append(uncertified, class)
		}
	}

	cert.Caveats = []string{
		"the bound holds on the calibration distribution only — the corpus's labeled decision stream — and says nothing about the selector's safety off it",
		"harm is per-unit any-violation — a tail either carried a corpus-excluded row or it did not — stricter than the row-level veto rate the binding benchmark reports",
		"decisions calibrate under the deterministic arm — an L3 resolver overlay can shift a prompt's effective class at runtime; these runs classify on the unoverlaid analysis",
		"render order is the producer's candidate order — live fetches are recency-ordered, the binding corpus's authored order stands in",
		"a class with no bound evidence or no certification does not constrain the composed value — the overlay covers the certified classes' distributions",
	}
	if m > 0 {
		cert.Caveats = append(cert.Caveats,
			fmt.Sprintf("each class walk ran at delta/%d = %.4f so the joint claim over certified classes holds at the certificate's delta", m, classDelta))
	}
	if len(unevidenced) > 0 {
		cert.Caveats = append(cert.Caveats,
			fmt.Sprintf("classes with no bound units in this corpus: %s", strings.Join(unevidenced, ", ")))
	}
	if len(uncertified) > 0 {
		cert.Caveats = append(cert.Caveats,
			fmt.Sprintf("classes that could not certify any tested value: %s", strings.Join(uncertified, ", ")))
	}
	if cert.CertifiedValue != nil {
		m := params.DefaultMemory()
		m.OpenRenderLimit = *cert.CertifiedValue
		cert.Overlay = map[string]any{
			"open_render_limit": *cert.CertifiedValue,
		}
		cert.OverlayBase = "params.DefaultMemory()"
		cert.ParamVersion = m.Version()
		cert.Guarantee = fmt.Sprintf(
			"with >=%.2f confidence, %s rate <=%.4f on the calibration distribution for every certified class (%s), %d stratified draws",
			1-delta, cert.Metric, alpha, strings.Join(certifiedClasses, ", "), totalDraws(cert.Classes))
	}
	return cert, nil
}

func totalDraws(classes []ClassCertificate) int {
	n := 0
	for _, c := range classes {
		n += c.Draws
	}
	return n
}

// bindingCellJSON is the eval-side decode of one labeled
// binding-benchmark row — the schema internal/agent's
// failure_binding.jsonl pins: a prompt, an on-disk shape the scope
// checks need, open-failure candidates, and the expected verdict
// per candidate. Re-declared here because the corpus schema is data
// the bench and the projector both read — the agent test type is
// the canonical shape.
type bindingCellJSON struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Dirs        []string `json:"dirs,omitempty"`
	FilesOnDisk []string `json:"files_on_disk,omitempty"`
	Candidates  []struct {
		Signature string   `json:"signature"`
		Cmd       string   `json:"cmd"`
		CWD       string   `json:"cwd"`
		Headline  string   `json:"headline,omitempty"`
		Files     []string `json:"files,omitempty"`
	} `json:"candidates"`
	Expect map[string]string `json:"expect"`
}

// LoadBindingDecisions reads a binding-benchmark jsonl corpus —
// '#'-comments and blank lines skipped, one cell per line — and
// runs the real selector over each cell, projecting its candidate
// decisions into calibration form. The cell is the unit, the
// deterministic scope class is the prompt's class, Bound is the
// uncapped admit, and Excluded is the cell's labeled rejection.
// The selector reads real disk state for scope/existence checks,
// so each cell materializes under a fresh temp dir exactly as the
// bench does; parent selects the scratch root ("" uses the OS
// temp dir).
func LoadBindingDecisions(path, parent string) ([]CalibrationDecision, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []CalibrationDecision
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var cell bindingCellJSON
		if err := json.Unmarshal(line, &cell); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		decs, err := projectBindingCell(cell, parent)
		if err != nil {
			return nil, err
		}
		out = append(out, decs...)
	}
	return out, sc.Err()
}

// projectBindingCell materializes one cell and projects its
// decisions — the temp dir is removed before returning.
func projectBindingCell(cell bindingCellJSON, parent string) ([]CalibrationDecision, error) {
	dir, err := os.MkdirTemp(parent, "binding-cell-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	for _, d := range cell.Dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	for _, fp := range cell.FilesOnDisk {
		full := filepath.Join(dir, fp)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			return nil, err
		}
	}
	var failures []cmdlog.Failure
	for _, c := range cell.Candidates {
		failures = append(failures, cmdlog.Failure{
			Signature: c.Signature, Cmd: c.Cmd, CWD: c.CWD,
			Headline: c.Headline, Files: c.Files,
			// Present-tense recency — the dataset has no staleness
			// dimension; anyPathNewer only fires on files newer than
			// the record.
			LastSeen: time.Now(),
		})
	}
	class := agent.ScopeClass(cell.Prompt, dir)
	_, decisions := agent.SimulateSelection(cell.Prompt, agent.MemoryPools{
		Open: failures,
	}, dir, agent.MemoryRenderLimits{}, nil)
	if len(decisions) != len(cell.Candidates) {
		return nil, fmt.Errorf("cell %s: %d decisions for %d candidates", cell.Name, len(decisions), len(cell.Candidates))
	}
	out := make([]CalibrationDecision, 0, len(decisions))
	for _, d := range decisions {
		want, ok := cell.Expect[d.Signature]
		if !ok {
			return nil, fmt.Errorf("cell %s: candidate %q has no expected verdict", cell.Name, d.Signature)
		}
		out = append(out, CalibrationDecision{
			Unit:     cell.Name,
			Class:    class,
			Pool:     d.Pool,
			Bound:    d.Admit,
			Excluded: want != "admit" && want != "admit_unique_scope",
		})
	}
	return out, nil
}
