package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/stretchr/testify/require"
)

// bindingCell is one labeled row of the offline binding benchmark —
// the #216 methodology gate. A cell pins a prompt, the on-disk shape
// the scope checks need, the open-failure candidates, and the exact
// selector verdict each candidate must earn. Cells are data: new
// resolver layers (L2 artifacts, L3 small-model) change expectations
// by editing cells, and the per-language report shows what each layer
// bought. A veto violation — a candidate admitted where a cell
// expected exclusion — fails the suite outright.
type bindingCell struct {
	Name   string `json:"name"`
	Lang   string `json:"lang"`
	Prompt string `json:"prompt"`
	// Dirs and FilesOnDisk are created inside the cell's temp workdir —
	// bare-dir and existence checks read real disk state.
	Dirs        []string           `json:"dirs,omitempty"`
	FilesOnDisk []string           `json:"files_on_disk,omitempty"`
	Candidates  []bindingCandidate `json:"candidates"`
	// Expect maps candidate signature → "admit" or a rejection reason.
	// Every candidate must appear — an unlabeled candidate is a cell
	// authoring error, not an anything-goes escape.
	Expect map[string]string `json:"expect"`
	// Settled optionally pins which layer produced the verdict —
	// "identifier", "lexicon", or "state". Omit when the layer is not
	// the point of the cell.
	Settled map[string]string `json:"settled,omitempty"`
	Note    string            `json:"note,omitempty"`
}

type bindingCandidate struct {
	Signature string   `json:"signature"`
	Cmd       string   `json:"cmd"`
	CWD       string   `json:"cwd"`
	Headline  string   `json:"headline,omitempty"`
	Files     []string `json:"files,omitempty"`
}

// TestBindingBenchmark scores selectOpenFailures against the labeled
// dataset — no agent runs, no providers. It reports per-language admit
// precision/recall, veto violations, and abstentions, and hard-fails
// on any veto violation or wrong verdict. Run it with -v to see the
// language table; the dataset lives in testdata/failure_binding.jsonl.
func TestBindingBenchmark(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/failure_binding.jsonl")
	require.NoError(t, err)
	defer f.Close()

	var cells []bindingCell
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var c bindingCell
		require.NoError(t, json.Unmarshal(line, &c))
		cells = append(cells, c)
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, cells, "binding benchmark dataset is empty")

	type langStats struct {
		cells          int
		decisions      int
		expectAdmits   int
		gotAdmits      int
		truePositives  int
		vetoViolations int
		abstains       int // cells where nothing admitted
	}
	perLang := map[string]*langStats{}

	for _, cell := range cells {
		t.Run(cell.Name, func(t *testing.T) {
			dir := t.TempDir()
			for _, d := range cell.Dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
			}
			for _, fp := range cell.FilesOnDisk {
				full := filepath.Join(dir, fp)
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
				require.NoError(t, os.WriteFile(full, []byte("package x\n"), 0o644))
			}

			var failures []cmdlog.Failure
			for _, c := range cell.Candidates {
				require.Contains(t, cell.Expect, c.Signature,
					"cell %s: candidate %q has no expected verdict", cell.Name, c.Signature)
				failures = append(failures, cmdlog.Failure{
					Signature: c.Signature,
					Cmd:       c.Cmd,
					CWD:       c.CWD,
					Headline:  c.Headline,
					Files:     c.Files,
					// Present-tense recency — the dataset has no
					// staleness dimension; anyPathNewer only fires
					// on files newer than the record.
					LastSeen: time.Now(),
				})
			}

			admitted, decisions := selectOpenFailures(cell.Prompt, failures, dir, 0)
			require.Len(t, decisions, len(failures),
				"cell %s: every candidate needs a decision", cell.Name)

			st := perLang[cell.Lang]
			if st == nil {
				st = &langStats{}
				perLang[cell.Lang] = st
			}
			st.cells++
			if len(admitted) == 0 {
				st.abstains++
			}
			admitSigs := map[string]bool{}
			for _, f := range admitted {
				admitSigs[f.Signature] = true
			}

			for _, d := range decisions {
				st.decisions++
				want := cell.Expect[d.Signature]
				wantAdmit := want == failAdmit
				if wantAdmit {
					st.expectAdmits++
				}
				if d.Admit {
					st.gotAdmits++
					if wantAdmit {
						st.truePositives++
					} else {
						st.vetoViolations++
					}
				}
				if wantAdmit {
					require.True(t, d.Admit,
						"cell %s sig %s: expected admit, got %s", cell.Name, d.Signature, d.Reason)
					require.Equal(t, failAdmit, d.Reason)
				} else {
					require.False(t, d.Admit,
						"cell %s sig %s: veto violation — admitted, expected %s",
						cell.Name, d.Signature, want)
					require.Equal(t, want, d.Reason,
						"cell %s sig %s: wrong rejection reason", cell.Name, d.Signature)
				}
				if layer, ok := cell.Settled[d.Signature]; ok {
					require.Equal(t, layer, d.SettledBy,
						"cell %s sig %s: wrong settling layer", cell.Name, d.Signature)
				}
			}
		})
	}

	// The acceptance gate: veto violations must be zero per language,
	// not just in aggregate — a single admitted exclusion is the mask
	// harm this resolver exists to bound.
	t.Run("report", func(t *testing.T) {
		for lang, st := range perLang {
			precision, recall := 0.0, 0.0
			if st.gotAdmits > 0 {
				precision = float64(st.truePositives) / float64(st.gotAdmits)
			}
			if st.expectAdmits > 0 {
				recall = float64(st.truePositives) / float64(st.expectAdmits)
			}
			t.Logf("lang=%s cells=%d decisions=%d admit_p=%.2f admit_r=%.2f veto_violations=%d abstains=%d",
				lang, st.cells, st.decisions, precision, recall, st.vetoViolations, st.abstains)
			require.Zero(t, st.vetoViolations,
				"lang %s: %d veto violations", lang, st.vetoViolations)
		}
	})
}
