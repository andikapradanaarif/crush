package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #409: an arm's hooks/tools blocks are schema-checked at manifest
// validation — a malformed block must fail here, not inside every
// run's app.New, or a manifest typo becomes an inconclusive loop.
func TestValidateArmFlags_Hooks(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	hookExp := func(hooks map[string]any) *Experiment {
		return &Experiment{Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {Config: ArmConfig{Hooks: hooks}},
		}}
	}

	require.NoError(t, manifest.ValidateArmFlags(hookExp(map[string]any{
		"PreToolUse": []any{map[string]any{"command": "bash scripts/lint.sh"}},
	})))
	// Paths that resolve inside the workdir after cleaning are allowed
	// — a/../b never leaves it; only a real escape fails.
	require.NoError(t, manifest.ValidateArmFlags(hookExp(map[string]any{
		"PreToolUse": []any{map[string]any{"command": "cat a/../b.txt"}},
	})))

	// Validation canonicalizes event spelling in place — merge slots
	// and the baseline hash see "PreToolUse", not the arm's spelling.
	exp := hookExp(map[string]any{
		"pre_tool_use": []any{map[string]any{"command": "bash x.sh"}},
	})
	require.NoError(t, manifest.ValidateArmFlags(exp))
	require.Contains(t, exp.Arms[ArmTreatment].Config.Hooks, "PreToolUse")
	require.NotContains(t, exp.Arms[ArmTreatment].Config.Hooks, "pre_tool_use")

	for name, hooks := range map[string]map[string]any{
		"unknown event":   {"NotAnEvent": []any{map[string]any{"command": "x"}}},
		"missing command": {"PreToolUse": []any{map[string]any{"name": "x"}}},
		"absolute path":   {"PreToolUse": []any{map[string]any{"command": "/usr/local/bin/lint.sh"}}},
		"tilde escape":    {"PreToolUse": []any{map[string]any{"command": "~/bin/lint.sh"}}},
		"dotdot escape":   {"PreToolUse": []any{map[string]any{"command": "bash ../outside/lint.sh"}}},
		"nested dotdot":   {"PreToolUse": []any{map[string]any{"command": "cat a/../../etc/passwd"}}},
		"cleaned dotdot":  {"PreToolUse": []any{map[string]any{"command": "bash ./../outside/x.sh"}}},
		"env-assign abs":  {"PreToolUse": []any{map[string]any{"command": "FOO=/etc/passwd ./x.sh"}}},
		"flag-value abs":  {"PreToolUse": []any{map[string]any{"command": "grep --file=/etc/p x"}}},
		"matcher on lifecycle": {"SessionStart": []any{
			map[string]any{"command": "x", "matcher": "bash"},
		}},
		"matcher bad regex": {"PreToolUse": []any{
			map[string]any{"command": "x", "matcher": "["},
		}},
		"double spelling": {
			"PreToolUse":   []any{map[string]any{"command": "x"}},
			"pre_tool_use": []any{map[string]any{"command": "y"}},
		},
		"unknown field": {"PreToolUse": []any{map[string]any{"command": "x", "bogus": 1}}},
		"bad timeout":   {"PreToolUse": []any{map[string]any{"command": "x", "timeout": -1}}},
		"wrong shape":   {"PreToolUse": "bash x.sh"},
	} {
		require.Error(t, manifest.ValidateArmFlags(hookExp(hooks)), name)
	}
}

func TestValidateArmFlags_Tools(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	toolExp := func(tools map[string]any) *Experiment {
		return &Experiment{Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {Config: ArmConfig{Tools: tools}},
		}}
	}

	require.NoError(t, manifest.ValidateArmFlags(toolExp(map[string]any{
		"grep": map[string]any{"timeout": 20000000000},
		"ls":   map[string]any{"max_depth": 4, "max_items": 100},
	})))

	for name, tools := range map[string]map[string]any{
		"unknown tool":     {"bogus": map[string]any{"x": 1}},
		"unknown field":    {"grep": map[string]any{"bogus": 1}},
		"negative depth":   {"ls": map[string]any{"max_depth": -1}},
		"negative timeout": {"glob": map[string]any{"timeout": -5}},
		"wrong shape":      {"grep": "fast"},
	} {
		require.Error(t, manifest.ValidateArmFlags(toolExp(tools)), name)
	}
}

// The acceptance criterion: arms differing only in a hook or a tool
// setting must key under distinct baselines — never pooled.
func TestArmKeyExtra_BaselineRotation(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{"memory_params": nil}}
	key := func(c ArmConfig) string {
		extra := c.KeyExtra()
		extra["$temperature"] = "0"
		return manifest.keyWith(c.Options, extra)
	}
	hookA := map[string]any{"PreToolUse": []any{map[string]any{"command": "bash a.sh"}}}
	hookB := map[string]any{"PreToolUse": []any{map[string]any{"command": "bash b.sh"}}}
	toolA := map[string]any{"grep": map[string]any{"timeout": 5000000000}}

	base := key(ArmConfig{Options: map[string]any{"memory_params": map[string]any{"x": 1}}})
	require.NotEqual(t, base, key(ArmConfig{
		Options: map[string]any{"memory_params": map[string]any{"x": 1}},
		Hooks:   hookA,
	}))
	require.NotEqual(t, base, key(ArmConfig{
		Options: map[string]any{"memory_params": map[string]any{"x": 1}},
		Tools:   toolA,
	}))
	require.NotEqual(t,
		key(ArmConfig{Hooks: hookA}),
		key(ArmConfig{Hooks: hookB}),
	)

	// Compatibility: an arm with no blocks contributes no dimensions —
	// its key is bit-identical to the pre-#409 call shape.
	empty := ArmConfig{}.KeyExtra()
	require.Empty(t, empty)
	require.Equal(t,
		manifest.keyWith(map[string]any{"memory_params": nil}, nil),
		manifest.keyWith(map[string]any{"memory_params": nil}, empty))
}

// WriteArmConfig renders the arm's blocks into .crush.json over a
// fixture: fixture keys survive, arm keys win per key.
func TestWriteArmConfig_HooksTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fixture := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"command": "bash setup.sh"}},
			// Same event as the arm's, under a different spelling —
			// the loader resolves both to PreToolUse, so the arm's
			// list must replace it, not coexist.
			"pre_tool_use": []any{map[string]any{"command": "bash fixture.sh"}},
		},
		"tools": map[string]any{
			"ls":   map[string]any{"max_items": 50},
			"glob": map[string]any{"timeout": 1000000000},
		},
	}
	raw, err := json.Marshal(fixture)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".crush.json"), raw, 0o644))

	arm := Arm{Config: ArmConfig{
		Hooks: map[string]any{"PreToolUse": []any{map[string]any{"command": "bash arm.sh"}}},
		Tools: map[string]any{
			"grep": map[string]any{"timeout": 20000000000},
			// Field-wise merge: the fixture's ls.max_items survives
			// alongside the arm's ls.max_depth.
			"ls": map[string]any{"max_depth": 4},
		},
	}}
	exp := &Experiment{Model: "p/m"}
	require.NoError(t, WriteArmConfig(dir, exp, arm, &FlagsManifest{Defaults: map[string]any{}}, "seed"))

	written, err := os.ReadFile(filepath.Join(dir, ".crush.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(written, &doc))

	hooks := doc["hooks"].(map[string]any)
	// The fixture's other event survives; the arm's event replaces the
	// fixture's differently-spelled same-event list wholesale — only
	// the canonical key remains.
	require.Len(t, hooks["SessionStart"], 1)
	require.NotContains(t, hooks, "pre_tool_use")
	require.Equal(t, "bash arm.sh", hooks["PreToolUse"].([]any)[0].(map[string]any)["command"])

	tools := doc["tools"].(map[string]any)
	require.Contains(t, tools, "glob") // Fixture's sub-tool survives.
	require.Equal(t, float64(20000000000), tools["grep"].(map[string]any)["timeout"])
	ls := tools["ls"].(map[string]any)
	require.Equal(t, float64(4), ls["max_depth"])  // Arm's.
	require.Equal(t, float64(50), ls["max_items"]) // Fixture's sibling field.
}

// A fixture crush.json hook on an arm-declared event must fail: the
// loader concatenates hook lists across config layers, so the
// fixture's entries would fire alongside the arm's replacing list.
func TestWriteArmConfig_CrushJsonHookCollision(t *testing.T) {
	t.Parallel()
	fixture := map[string]any{
		"hooks": map[string]any{"PreToolUse": []any{map[string]any{"command": "bash f.sh"}}},
	}
	arm := Arm{Config: ArmConfig{
		Hooks: map[string]any{"pre_tool_use": []any{map[string]any{"command": "bash a.sh"}}},
	}}
	exp := &Experiment{Model: "p/m"}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	dir := t.TempDir()
	raw, err := json.Marshal(fixture)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crush.json"), raw, 0o644))
	require.ErrorContains(t, WriteArmConfig(dir, exp, arm, manifest, "seed"), "crush.json hooks")

	// A fixture event the arm does NOT declare is a constant across
	// arms — allowed, same as non-flag fixture options.
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crush.json"), raw, 0o644))
	other := Arm{Config: ArmConfig{
		Hooks: map[string]any{"SessionStart": []any{map[string]any{"command": "bash s.sh"}}},
	}}
	require.NoError(t, WriteArmConfig(dir, exp, other, manifest, "seed"))
}
