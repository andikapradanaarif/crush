package eval

import (
	"encoding/json"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
)

// envRefRe extracts variable names from shell-style templates:
// $VAR, ${VAR}, and ${VAR:-default} / ${VAR:?msg} / ${VAR:+alt}
// forms all name their variable at the same position. Command
// substitution ($(cmd)) starts with '(' so it can't match; variable
// references inside the substituted command still surface — the
// pattern scans the whole template text.
var envRefRe = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}|([A-Za-z_][A-Za-z0-9_]*))`)

// fixedChildEnv is the minimal ambient set every eval child needs:
// exec lookup, the pinned HOME (subprocessEnv re-pins it anyway), and
// the temp root. Everything else must be manifest-referenced — an
// ambient credential the manifest never named is exactly the leak
// class that selected a dead small model in #333.
func fixedChildEnv() []string {
	base := []string{"PATH", "HOME", "TMPDIR"}
	if runtime.GOOS == "windows" {
		// Process spawn, DNS, and temp resolution on Windows need
		// more than the POSIX trio — SystemRoot is effectively
		// mandatory for a working child.
		base = append(base, "SYSTEMROOT", "SystemRoot", "COMSPEC", "PATHEXT",
			"TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"HOMEDRIVE", "HOMEPATH", "USERNAME")
	}
	return base
}

// scanEnvRefs pulls every $VAR / ${VAR...} name out of one JSON
// document — a providers block or an arm's options map. Marshaling
// first means refs hidden in nested headers, base_url templates, or
// option strings surface without walking the structure by hand.
func scanEnvRefs(doc any, out map[string]bool) {
	if doc == nil {
		return
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	for _, m := range envRefRe.FindAllStringSubmatch(string(data), -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		out[name] = true
	}
}

// envAllowlist returns the sorted set of parent-env names a manifest
// permits into eval children (#345): the fixed minimal set plus every
// variable the manifest references —
//
//   - credential/endpoint templates in the declared providers block
//     ($LLM_API_KEY-style values anywhere in it),
//   - env refs inside any arm's options delta,
//   - the catwalk catalog templates (api_key, api_endpoint,
//     default_headers) of providers the experiment pins via model /
//     small_model but doesn't declare — builtin providers get their
//     credential shape from the catalog, so the catalog is where
//     their referenced names live.
//
// A provider neither declared nor pinned contributes nothing, so its
// ambient key is scrubbed and it can never activate — that is the
// point. A catalog lookup failure degrades to manifest-refs-only:
// undeclared pins then fail loudly in preflight ("resolves empty")
// rather than silently inheriting.
func envAllowlist(exp *Experiment) []string {
	set := map[string]bool{}
	for _, k := range fixedChildEnv() {
		set[k] = true
	}
	scanEnvRefs(exp.Providers, set)
	for _, arm := range exp.Arms {
		scanEnvRefs(arm.Config.Options, set)
	}
	// Undeclared pins borrow the catalog's credential templates.
	pinned := map[string]bool{}
	for _, modelPin := range []string{exp.Model, exp.SmallModel} {
		if id, _, ok := strings.Cut(modelPin, "/"); ok && id != "" {
			pinned[id] = true
		}
	}
	known, err := config.Providers(&config.Config{
		Options: &config.Options{DisableProviderAutoUpdate: true},
	})
	if err == nil {
		for _, kp := range known {
			if !pinned[string(kp.ID)] {
				continue
			}
			if _, declared := exp.Providers[string(kp.ID)]; declared {
				continue
			}
			scanEnvRefs(kp.APIKey, set)
			scanEnvRefs(kp.APIEndpoint, set)
			scanEnvRefs(kp.DefaultHeaders, set)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
