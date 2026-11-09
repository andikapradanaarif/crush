package skills

import (
	"context"
	"fmt"
)

// OverrideStore reads repository-scoped skill toggle overrides.
type OverrideStore interface {
	SkillsDisabled(context.Context) ([]string, error)
	SkillsEnabled(context.Context) ([]string, error)
}

// ResolveActive applies current config and repository overrides to the full
// discovered skill set. Catalogs must not filter a startup active snapshot:
// it no longer contains skills that a repository override can re-enable.
func ResolveActive(ctx context.Context, all []*Skill, configDisabled []string, store OverrideStore) ([]*Skill, error) {
	disabled, err := store.SkillsDisabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("list disabled skills: %w", err)
	}
	enabled, err := store.SkillsEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled skills: %w", err)
	}
	return FilterWithOverrides(all, configDisabled, disabled, enabled), nil
}

// FilterWithOverrides resolves config and repository toggle precedence before
// removing skills. Repository enable overrides take precedence over disables.
func FilterWithOverrides(all []*Skill, configDisabled, repoDisabled, repoEnabled []string) []*Skill {
	disabled := make(map[string]bool, len(configDisabled)+len(repoDisabled))
	for _, name := range configDisabled {
		disabled[name] = true
	}
	for _, name := range repoDisabled {
		disabled[name] = true
	}
	for _, name := range repoEnabled {
		delete(disabled, name)
	}
	active := make([]*Skill, 0, len(all))
	for _, skill := range all {
		if !disabled[skill.Name] {
			active = append(active, skill)
		}
	}
	return active
}
