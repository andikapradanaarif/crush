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

	for name, hooks := range map[string]map[string]any{
		"unknown event":   {"NotAnEvent": []any{map[string]any{"command": "x"}}},
		"missing command": {"PreToolUse": []any{map[string]any{"name": "x"}}},
		"absolute path":   {"PreToolUse": []any{map[string]any{"command": "/usr/local/bin/lint.sh"}}},
		"tilde escape":    {"PreToolUse": []any{map[string]any{"command": "~/bin/lint.sh"}}},
		"dotdot escape":   {"PreToolUse": []any{map[string]any{"command": "bash ../outside/lint.sh"}}},
		"unknown field":   {"PreToolUse": []any{map[string]any{"command": "x", "bogus": 1}}},
		"bad timeout":     {"PreToolUse": []any{map[string]any{"command": "x", "timeout": -1}}},
		"wrong shape":     {"PreToolUse": "bash x.sh"},
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
			"PreToolUse":   []any{map[string]any{"command": "bash fixture.sh"}},
		},
		"tools": map[string]any{"glob": map[string]any{"timeout": 1000000000}},
	}
	raw, err := json.Marshal(fixture)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".crush.json"), raw, 0o644))

	arm := Arm{Config: ArmConfig{
		Hooks: map[string]any{"PreToolUse": []any{map[string]any{"command": "bash arm.sh"}}},
		Tools: map[string]any{"grep": map[string]any{"timeout": 20000000000}},
	}}
	exp := &Experiment{Model: "p/m"}
	require.NoError(t, WriteArmConfig(dir, exp, arm, &FlagsManifest{Defaults: map[string]any{}}, "seed"))

	written, err := os.ReadFile(filepath.Join(dir, ".crush.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(written, &doc))

	hooks := doc["hooks"].(map[string]any)
	// The fixture's other event survives; the arm's event replaces the
	// fixture's list for the same event wholesale.
	require.Len(t, hooks["SessionStart"], 1)
	require.Equal(t, "bash arm.sh", hooks["PreToolUse"].([]any)[0].(map[string]any)["command"])

	tools := doc["tools"].(map[string]any)
	require.Contains(t, tools, "glob") // Fixture's sub-tool survives.
	require.Equal(t, float64(20000000000), tools["grep"].(map[string]any)["timeout"])
}
