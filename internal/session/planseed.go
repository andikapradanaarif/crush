package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PlanStartMarker is the sentinel the plan agent emits on its own line at
// the very start of its final response, so observers can tell the plan
// document apart from the agent's intermediate exploratory replies.
const PlanStartMarker = "<!-- CRUSH_PLAN_START -->"

// PlanReadyMarker is the sentinel the plan agent emits on its own line at
// the end of its final response to signal that the plan is ready for
// execution.
const PlanReadyMarker = "<!-- CRUSH_PLAN_READY -->"

// PlanItemsFence is the fenced-block language tag the plan agent uses to
// emit the plan's typed item list — the machine-readable counterpart of
// the human-facing plan text. The block sits inside the marker payload,
// immediately before the ready marker, and seeds the session's PlanItems
// on approval so the approved plan is the artifact the harness checks.
const PlanItemsFence = "crush-plan-items"

// PlanMarkerPresent reports whether text contains the given plan sentinel
// on a line by itself. An own-line check (rather than a substring match)
// avoids a false positive when the agent merely mentions the marker inside
// explanatory prose, while still tolerating trailing whitespace or notes
// after it. Inline-code backticks around the marker count as a marker line
// too: some models wrap the marker despite the prompt asking for plain
// text.
func PlanMarkerPresent(text, marker string) bool {
	for line := range strings.Lines(text) {
		if PlanMarkerLine(line, marker) {
			return true
		}
	}
	return false
}

// PlanMarkerLine reports whether the line consists solely of the given
// plan sentinel, optionally wrapped in inline-code backticks and
// whitespace.
func PlanMarkerLine(line, marker string) bool {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "`"))
	return trimmed == marker
}

// planSeedItem is the wire shape of one entry in the typed items block —
// the same vocabulary the todos tool accepts, minus status (seeds always
// land pending; execution owns status from then on).
type planSeedItem struct {
	Key            string   `json:"key"`
	Content        string   `json:"content"`
	ActiveForm     string   `json:"active_form,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
	EvidenceChecks []string `json:"evidence_checks,omitempty"`
	EvidencePaths  []string `json:"evidence_paths,omitempty"`
}

// ParsePlanSeed extracts the typed item list from a plan-mode response.
// It returns (nil, nil) when the text carries no items block, and an error
// only when a block is present but malformed — callers deciding whether
// approval happened should treat a parse failure as "no seeds", not as a
// failed approval.
func ParsePlanSeed(text string) ([]PlanItem, error) {
	block, ok := planItemsBlock(text)
	if !ok {
		return nil, nil
	}
	var raw []planSeedItem
	if err := json.Unmarshal([]byte(block), &raw); err != nil {
		return nil, fmt.Errorf("parsing plan items block: %w", err)
	}

	// First pass: the valid key set. depends_on edges referencing keys
	// that never landed (duplicates, empty-content items) are dropped
	// rather than failing the whole seed.
	keys := map[string]bool{}
	for _, item := range raw {
		if item.Key != "" && strings.TrimSpace(item.Content) != "" && !keys[item.Key] {
			keys[item.Key] = true
		}
	}

	seenIDs := map[string]bool{}
	seenKeys := map[string]bool{}
	items := make([]PlanItem, 0, len(raw))
	for _, item := range raw {
		// The emitted schema requires a key — it is the item's identity
		// across re-approval merges, so keyless entries are dropped
		// rather than degrading the merge to content hashing.
		if strings.TrimSpace(item.Content) == "" || strings.TrimSpace(item.Key) == "" {
			continue
		}
		if seenKeys[item.Key] {
			continue
		}
		seenKeys[item.Key] = true
		id := MintPlanItemID(item.Key, item.Content)
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		var deps []string
		for _, dep := range item.DependsOn {
			if keys[dep] && dep != item.Key {
				deps = append(deps, MintPlanItemID(dep, ""))
			}
		}
		items = append(items, PlanItem{
			ID:             id,
			Key:            item.Key,
			Content:        item.Content,
			Status:         PlanItemPending,
			ActiveForm:     item.ActiveForm,
			DependsOn:      deps,
			EvidenceChecks: item.EvidenceChecks,
			EvidencePaths:  item.EvidencePaths,
		})
	}
	return items, nil
}

// planItemsBlock returns the contents of the first fenced code block
// tagged with the plan-items language, tolerating indented fences and
// extra whitespace on the info string. Other fenced blocks are tracked
// so a plan that embeds an example of its own items fence inside a
// different code block is not falsely parsed.
func planItemsBlock(text string) (string, bool) {
	var block strings.Builder
	inBlock := false
	inFence := false
	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if strings.HasPrefix(trimmed, "```") {
				return block.String(), true
			}
			// strings.Lines yields lines with their terminators, so
			// no extra newline is needed.
			block.WriteString(line)
			continue
		}
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence && isPlanItemsFenceLine(trimmed) {
			inBlock = true
		}
	}
	return "", false
}

func isFenceLine(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "```")
	return ok && strings.TrimSpace(strings.TrimLeft(rest, "`")) != PlanItemsFence
}

func isPlanItemsFenceLine(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "```")
	return ok && strings.TrimSpace(strings.TrimLeft(rest, "`")) == PlanItemsFence
}

// StripPlanItems removes plan-items fenced blocks so the machine-readable
// seed never renders in the plan card or chat output. The block is
// self-delimiting, so stripping does not disturb surrounding prose; an
// unclosed block drops the remainder of the text, which is correct during
// streaming — the block is emitted last by design.
func StripPlanItems(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	inBlock := false
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if strings.HasPrefix(trimmed, "```") {
				inBlock = false
			}
			continue
		}
		if isFenceLine(trimmed) {
			inFence = !inFence
		} else if !inFence && isPlanItemsFenceLine(trimmed) {
			inBlock = true
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// MergePlanSeed folds a freshly approved plan's items into the session's
// existing list. The result is the seed list in seed order — items the
// revision dropped are gone — but an item whose minted ID survives the
// revision keeps its stored status: a re-approved plan must not resurrect
// completed work as pending. Keyed seeds hash their key, so reworded
// content still merges; keyless seeds hash content, so a reworded keyless
// item mints a new ID and the stale one is dropped — the merge degrades
// to replace only where identity could not survive.
//
// Known limit: the merge cannot distinguish seed-origin items from ones
// the coder appended mid-execution, so re-approving a plan while work is
// in flight discards the coder's tracking additions. Approval normally
// precedes execution, so the loss window is narrow; an explicit
// origin marker on PlanItem would close it if it ever matters.
func MergePlanSeed(existing, seeds []PlanItem) []PlanItem {
	statusByID := make(map[string]PlanItemStatus, len(existing))
	for _, item := range existing {
		statusByID[item.ID] = item.Status
	}
	merged := make([]PlanItem, len(seeds))
	for i, item := range seeds {
		if status, ok := statusByID[item.ID]; ok {
			item.Status = status
		}
		merged[i] = item
	}
	return merged
}

// PlanItemsBound reports whether the list counts as an evidence-bound
// declaration: non-empty, with every item binding at least one evidence
// check or path. Seeded or handwritten bare items do not satisfy it — a
// plan the gate can lean on must name what the work touches.
func PlanItemsBound(items []PlanItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if len(item.EvidenceChecks) == 0 && len(item.EvidencePaths) == 0 {
			return false
		}
	}
	return true
}
