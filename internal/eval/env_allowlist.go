package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
)

// envRefRe extracts variable names from shell-style templates. Two
// shapes, scanned independently across the template text:
//
//   - ${...} braced: ${VAR}, ${#VAR}, ${!VAR}, ${VAR:-default} and
//     friends name their variable right after the brace (and any
//     length/indirect sigil). Nested forms like ${VAR:-${OTHER}}
//     surface too — the ${OTHER} inside matches on its own.
//   - $VAR bare.
//
// Command substitution ($(cmd)) starts with '(' so it can't match;
// variable references inside the substituted command still surface.
var envRefRe = regexp.MustCompile(`\$(?:\{[#?!]*([A-Za-z_][A-Za-z0-9_]*)|([A-Za-z_][A-Za-z0-9_]*))`)

// passthroughNameRe is the env_passthrough grammar: an env var name
// with an optional single trailing '*' admitting the family
// ("CLOUDSDK_*" passes CLOUDSDK_CONFIG). A bare '*' is not a name.
var passthroughNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\*?$`)

// validatePassthroughName rejects env_passthrough entries that can
// never admit anything or smuggle a value past the names-only
// contract.
func validatePassthroughName(name string) error {
	if !passthroughNameRe.MatchString(name) {
		return fmt.Errorf("%q is not an env var name (optionally ending in one '*')", name)
	}
	if strings.HasPrefix(name, "CRUSH_") || strings.HasPrefix(name, "EVAL_") {
		return fmt.Errorf("%q is reserved — harness-prefixed vars are stripped before the allowlist runs", name)
	}
	return nil
}

// ambientCredentialEnv names the out-of-band credential chains for
// providers whose catalog entries carry no api_key template — the
// regex can't find names that never appear in a template, so this
// map is the source of truth for SDK-native auth. Keyed by the
// pinned provider id; admission happens only when that provider is
// the measured pin, which is the referenced credential surface.
var ambientCredentialEnv = map[string][]string{
	// hyper resolves HYPER_API_KEY out-of-band inside the child
	// (config.ResolveHyperAPIKey) — its catalog api_key is null.
	"hyper": {"HYPER_API_KEY"},
	// The AWS SDK default chain — credential, region, profile, and
	// role/web-identity sources bedrock resolves ambiently.
	"bedrock":        awsCredentialEnv,
	"bedrock-europe": awsCredentialEnv,
	// Google ADC — service-account file and the gcloud project
	// selectors vertex resolves ambiently.
	"vertexai": gcpCredentialEnv,
	// copilot authenticates via an on-disk OAuth token under HOME —
	// pinned-hermetic evals can't reach it through env either way.
}

// awsCredentialEnv is the documented AWS SDK Go v2 environment
// surface: static credentials, the session token, region selectors,
// profile/config file locations, and the web-identity / container
// credential providers.
var awsCredentialEnv = []string{
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_REGION", "AWS_DEFAULT_REGION",
	"AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_SDK_LOAD_CONFIG",
	"AWS_EC2_METADATA_SERVICE_ENDPOINT",
}

// gcpCredentialEnv is the documented Application Default Credentials
// surface: the service-account file plus the project/quota-project
// selectors the gcloud toolchain resolves.
var gcpCredentialEnv = []string{
	"GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT",
	"CLOUDSDK_CORE_PROJECT", "CLOUDSDK_PROJECT", "CLOUDSDK_CONFIG",
	"GOOGLE_CLOUD_QUOTA_PROJECT",
}

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
//     their referenced names live,
//   - the ambient credential chains of pinned SDK-native providers
//     (ambientCredentialEnv) — hyper's HYPER_API_KEY, the AWS SDK
//     chain behind bedrock, Google ADC behind vertex — providers
//     whose credentials never appear in any template.
//
// A provider neither declared nor pinned contributes nothing, so its
// ambient key is scrubbed and it can never activate — that is the
// point. A catalog lookup failure degrades to manifest-refs-only:
// undeclared pins then fail loudly in preflight ("resolves empty")
// rather than silently inheriting.
//
// Surfaces no $-ref can name — $(command) helpers' ambient vars
// (CLOUDSDK_*, PASSWORD_STORE_*), proxy/toolchain vars (HTTP_PROXY,
// GOPROXY, SSH_AUTH_SOCK) — admit through env_passthrough (#373):
// declared names, recorded like every other admission. What stays
// cut is ambient inheritance itself — a passthrough without a
// declaration is the #333 leak class.
func envAllowlist(exp *Experiment) []string {
	set := map[string]bool{}
	for _, k := range fixedChildEnv() {
		set[k] = true
	}
	scanEnvRefs(exp.Providers, set)
	for _, arm := range exp.Arms {
		scanEnvRefs(arm.Config.Options, set)
	}
	// Declared passthrough names merge verbatim — the pattern itself
	// (e.g. CLOUDSDK_*) is the honest record of what was admitted.
	for _, name := range exp.EnvPassthrough {
		set[name] = true
	}
	// Pinned providers get two admissions: the ambient credential
	// chain for SDK-native auth (hyper, bedrock, vertex) and — when
	// undeclared — the catalog's credential templates.
	pinned := map[string]bool{}
	for _, modelPin := range []string{exp.Model, exp.SmallModel} {
		if id, _, ok := strings.Cut(modelPin, "/"); ok && id != "" {
			pinned[id] = true
		}
	}
	for id := range pinned {
		for _, k := range ambientCredentialEnv[id] {
			set[k] = true
		}
	}
	known, _ := config.Providers(&config.Config{
		Options: &config.Options{DisableProviderAutoUpdate: true},
	})
	// Iterate whatever the catalog returned even alongside an error —
	// the memoized fetch must not poison the entries that did load
	// (same advisory-error convention as preflight's known/catErr).
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
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
