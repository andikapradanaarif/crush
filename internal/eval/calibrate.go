package eval

import (
	"database/sql"
	"fmt"
	"math"
	"slices"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/params"
)

// Learn-then-Test offline calibration for params.Memory values
// (#295). For one parameter and one binary harm metric, each
// candidate value's harm rate is estimated on settled referent
// episodes and tested against "rate > alpha" with a Hoeffding–Bentkus
// p-value. Candidates are tested in a FIXED sequence ordered
// most-conservative → least, which controls the family-wise error
// rate at delta with no multiplicity correction (Angelopoulos et
// al., LTT §4). The least conservative value that still rejects the
// unsafe null is certified; certifying nothing is a valid outcome —
// the caller keeps the default, the Seldonian ship/no-ship shape.

// CalibrationEpisode is one settled referent episode in calibration
// form — the eval-local projection of the referent_episodes row so
// the LTT core is testable without a database.
type CalibrationEpisode struct {
	ID          int64
	Phrase      string
	Target      string
	SessionID   string
	Verdict     string
	Committed   sql.NullInt64
	HashChanged sql.NullInt64
	Suggested   bool
}

// mappingKey groups episodes onto the referent mapping they would
// render under any candidate floor.
func (e CalibrationEpisode) mappingKey() string {
	return e.Phrase + "\x00" + e.Target
}

// StratifyOnePerSession keeps the earliest episode of each session.
// LTT p-values assume exchangeable draws; episodes inside one session
// share the session's outcome context, so they are correlated draws
// of the same event, not independent evidence. Episodes arrive
// id-ordered, so first appearance is the earliest.
func StratifyOnePerSession(eps []CalibrationEpisode) []CalibrationEpisode {
	seen := make(map[string]bool, len(eps))
	out := eps[:0]
	for _, e := range eps {
		if seen[e.SessionID] {
			continue
		}
		seen[e.SessionID] = true
		out = append(out, e)
	}
	return out
}

// hbUpperPValue is the Hoeffding–Bentkus p-value for the null
// "harm rate > alpha" given k observed harms in n exchangeable draws
// (LTT's recommended fixed-sequence p-value). Small p rejects the
// null — the candidate is safe. It is the min of the Hoeffding
// left-tail bound and the e-inflated exact Bentkus tail; both are
// valid simultaneously, so the min stays a valid p-value.
func hbUpperPValue(k, n int, alpha float64) float64 {
	if n <= 0 {
		return 1
	}
	rhat := float64(k) / float64(n)
	hoeffding := 1.0
	if rhat < alpha {
		hoeffding = math.Exp(-2 * float64(n) * (alpha - rhat) * (alpha - rhat))
	}
	bentkus := math.E * cmdlog.BinomialTailLE(k, n, alpha)
	return min(hoeffding, min(bentkus, 1))
}

// CandidateResult records one grid point's test outcome.
type CandidateResult struct {
	Value     int     `json:"value"`
	Admits    int     `json:"admits"`
	Harms     int     `json:"harms"`
	RiskHat   float64 `json:"risk_hat"`
	PValue    float64 `json:"p_value"`
	Certified bool    `json:"certified"`
}

// Certificate is the calibrate run's auditable output: the tested
// sequence, per-candidate evidence, the certified value (or its
// honest absence), and the versioned overlay that would apply it.
// The guarantee is scoped to the calibration distribution — it says
// nothing about deployments the episode set doesn't represent.
type Certificate struct {
	Param          string            `json:"param"`
	Metric         string            `json:"metric"`
	Alpha          float64           `json:"alpha"`
	Delta          float64           `json:"delta"`
	Episodes       int               `json:"episodes"`
	Draws          int               `json:"draws"`
	Candidates     []CandidateResult `json:"candidates"`
	CertifiedValue *int              `json:"certified_value"`
	Overlay        map[string]any    `json:"overlay,omitempty"`
	ParamVersion   string            `json:"param_version,omitempty"`
	Guarantee      string            `json:"guarantee"`
}

const (
	// CalibrateParamPromoteHits is the referent_promote_hits knob.
	CalibrateParamPromoteHits = "referent_promote_hits"
	// CalibrateMetricWrongReferent is the wrong-referent admit rate:
	// among referent admits a candidate floor would emit, the share
	// whose episode outcome is bad under the shared outcome rule.
	CalibrateMetricWrongReferent = "wrong_referent_admit"
)

// CalibratePromoteHits runs the fixed-sequence LTT test for
// ReferentPromoteHits. grid must list candidate floors
// most-conservative → least (descending); the walk stops at the
// first candidate it cannot certify, so every reported certified
// value is safe at delta and the chosen one is the least
// conservative of them.
//
// A candidate h's admit population is the stratified episodes whose
// mapping clears the floor — the same clean-acceptance
// distinct-session count the live Promote check uses — computed
// leave-one-out so an episode's own acceptance can't admit itself.
// An admit is harmful when the episode's shared outcome is bad: the
// certificate's risk is the wrong-referent admit rate, not the
// channel's overall bad-outcome rate.
func CalibratePromoteHits(eps []CalibrationEpisode, grid []int, alpha, delta float64) (Certificate, error) {
	if len(grid) == 0 {
		return Certificate{}, fmt.Errorf("empty candidate grid")
	}
	if alpha <= 0 || alpha >= 1 || delta <= 0 || delta >= 1 {
		return Certificate{}, fmt.Errorf("alpha and delta must be in (0,1): alpha=%v delta=%v", alpha, delta)
	}
	grid = slices.Clone(grid)
	slices.Sort(grid)
	slices.Reverse(grid)
	grid = slices.Compact(grid)

	// Clean-acceptance distinct sessions per mapping — the live
	// CountCleanReferentAcceptances evidence — plus, for LOO
	// renderability, the same count restricted to each session so an
	// episode knows whether its own session's evidence is sole.
	type mappingEvidence struct {
		sessions    map[string]bool
		sessionOnly map[string]int // session → clean episode count
	}
	evidence := make(map[string]*mappingEvidence)
	for _, e := range eps {
		if e.Verdict != cmdlog.ReferentAccepted || e.Suggested {
			continue
		}
		m := evidence[e.mappingKey()]
		if m == nil {
			m = &mappingEvidence{sessions: map[string]bool{}, sessionOnly: map[string]int{}}
			evidence[e.mappingKey()] = m
		}
		m.sessions[e.SessionID] = true
		m.sessionOnly[e.SessionID]++
	}

	draws := StratifyOnePerSession(eps)
	renders := func(e CalibrationEpisode, h int) bool {
		m := evidence[e.mappingKey()]
		if m == nil {
			return false
		}
		count := len(m.sessions)
		if e.Verdict == cmdlog.ReferentAccepted && !e.Suggested && m.sessionOnly[e.SessionID] == 1 {
			count--
		}
		return count >= h
	}

	cert := Certificate{
		Param:    CalibrateParamPromoteHits,
		Metric:   CalibrateMetricWrongReferent,
		Alpha:    alpha,
		Delta:    delta,
		Episodes: len(eps),
		Draws:    len(draws),
	}
	for _, h := range grid {
		admits, harms := 0, 0
		for _, e := range draws {
			if !renders(e, h) {
				continue
			}
			admits++
			if !cmdlog.ReferentEpisodeOutcome(e.Verdict, e.Committed, e.HashChanged) {
				harms++
			}
		}
		p := hbUpperPValue(harms, admits, alpha)
		res := CandidateResult{
			Value:  h,
			Admits: admits,
			Harms:  harms,
			PValue: p,
		}
		if admits > 0 {
			res.RiskHat = float64(harms) / float64(admits)
		}
		res.Certified = p <= delta && admits > 0
		cert.Candidates = append(cert.Candidates, res)
		if !res.Certified {
			break
		}
		cert.CertifiedValue = new(int)
		*cert.CertifiedValue = h
	}
	if cert.CertifiedValue != nil {
		m := params.DefaultMemory()
		m.ReferentPromoteHits = *cert.CertifiedValue
		cert.Overlay = map[string]any{
			"referent_promote_hits": *cert.CertifiedValue,
		}
		cert.ParamVersion = m.Version()
	}
	cert.Guarantee = fmt.Sprintf(
		"with >=%.2f confidence, %s rate <=%.4f on the calibration distribution (%d stratified draws, one per session)",
		1-delta, cert.Metric, alpha, cert.Draws)
	return cert, nil
}
