package toolclass

import (
	"path/filepath"
	"slices"
	"strings"
)

// Command kinds reported by CommandKind. "run" covers dev-server /
// app-launch invocations; verification invocations split into build,
// test, and lint.
const (
	CommandKindBuild = "build"
	CommandKindTest  = "test"
	CommandKindLint  = "lint"
	CommandKindRun   = "run"
	CommandKindOther = "other"
)

// buildTestCommands maps a command name to the subcommands that mark a
// build, test, or lint invocation. An empty slice means the command is
// itself a build/test tool and needs no subcommand. The "run" and
// "exec" subcommands are handled separately via buildTestRunTargets.
var buildTestCommands = map[string][]string{
	"go":            {"build", "test", "vet"},
	"cargo":         {"build", "test", "check", "clippy"},
	"npm":           {"test", "ci"},
	"pnpm":          {"test", "build", "lint"},
	"yarn":          {"test", "build", "lint"},
	"bun":           {"test", "build"},
	"deno":          {"test", "check", "lint"},
	"dotnet":        {"build", "test"},
	"mvn":           {"compile", "test", "verify", "package"},
	"gradle":        {"build", "test", "check"},
	"gradlew":       {"build", "test", "check"},
	"cmake":         {"--build"},
	"pytest":        {},
	"tsc":           {},
	"make":          {},
	"task":          {},
	"just":          {},
	"ctest":         {},
	"golangci-lint": {},
	"staticcheck":   {},
}

// buildTestRunTargets lists script names accepted after a "run"
// subcommand (e.g. "npm run build").
var buildTestRunTargets = []string{
	"build", "test", "lint", "check", "typecheck", "type-check", "tsc", "ci",
}

// commandWrappers are leading words that wrap the real command.
var commandWrappers = map[string]bool{
	"sudo": true, "env": true, "time": true,
	"nice": true, "nohup": true, "command": true, "exec": true,
}

// wrapperFlagArgs are wrapper flags that consume a following value
// argument (e.g. "env -u NAME", "nice -n 5", "sudo -u root"). Keyed by
// flag name without leading dashes.
var wrapperFlagArgs = map[string]bool{
	"u": true, "g": true, "h": true, "unset": true,
	"C": true, "chdir": true, "S": true, "split-string": true,
	"P": true, "alternate-argv": true,
	"n": true, "adjustment": true,
}

// bareCommandKinds are the kinds for tools that need no subcommand —
// the tool name itself carries the meaning.
var bareCommandKinds = map[string]string{
	"pytest":        CommandKindTest,
	"ctest":         CommandKindTest,
	"tsc":           CommandKindLint,
	"golangci-lint": CommandKindLint,
	"staticcheck":   CommandKindLint,
	"make":          CommandKindBuild,
	"task":          CommandKindBuild,
	"just":          CommandKindBuild,
}

// subcommandKinds map a recognized build/test subcommand to its kind.
var subcommandKinds = map[string]string{
	"build":      CommandKindBuild,
	"--build":    CommandKindBuild,
	"compile":    CommandKindBuild,
	"package":    CommandKindBuild,
	"test":       CommandKindTest,
	"ci":         CommandKindTest,
	"verify":     CommandKindTest,
	"lint":       CommandKindLint,
	"check":      CommandKindLint,
	"clippy":     CommandKindLint,
	"vet":        CommandKindLint,
	"typecheck":  CommandKindLint,
	"type-check": CommandKindLint,
	"tsc":        CommandKindLint,
}

// isEnvAssignment reports whether a leading field is a KEY=VALUE env
// assignment rather than the command name.
func isEnvAssignment(field string) bool {
	if strings.HasPrefix(field, "-") {
		return false
	}
	idx := strings.IndexByte(field, '=')
	if idx <= 0 {
		return false
	}
	for i := range idx {
		c := field[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' ||
			'0' <= c && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// commandSegments splits a command line on shell chaining operators so
// each segment classifies independently: "cd x && go test" still
// matches the go test segment.
func commandSegments(command string) [][]string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n'
	})
	out := make([][]string, 0, len(segments))
	for _, seg := range segments {
		fields := strings.Fields(seg)
		// Skip leading env assignments, command wrappers, and wrapper
		// flags so "CGO_ENABLED=0 go test", "env -i go test", or
		// "nice -n 5 make" still classify.
		sawWrapper := false
	fieldsLoop:
		for len(fields) > 0 {
			f := fields[0]
			switch {
			case isEnvAssignment(f):
				fields = fields[1:]
			case commandWrappers[f]:
				sawWrapper = true
				fields = fields[1:]
			case sawWrapper && strings.HasPrefix(f, "-"):
				fields = fields[1:]
				if wrapperFlagArgs[strings.TrimLeft(f, "-")] && len(fields) > 0 {
					fields = fields[1:]
				}
			default:
				break fieldsLoop
			}
		}
		out = append(out, fields)
	}
	return out
}

// matchBuildTest returns the tool name and effective subcommand of the
// first segment that invokes a known build/test/lint tool.
func matchBuildTest(command string) (name, sub string, ok bool) {
	for _, fields := range commandSegments(command) {
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(fields[0]), ".exe")
		subs, known := buildTestCommands[name]
		if !known {
			continue
		}
		if len(subs) == 0 {
			return name, "", true
		}
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "run" || fields[1] == "exec" {
			if len(fields) >= 3 && slices.Contains(buildTestRunTargets, fields[2]) {
				return name, fields[2], true
			}
			continue
		}
		if slices.Contains(subs, fields[1]) {
			return name, fields[1], true
		}
	}
	return "", "", false
}

// IsBuildOrTestCommand reports whether command invokes a known build,
// test, or lint tool.
func IsBuildOrTestCommand(command string) bool {
	_, _, ok := matchBuildTest(command)
	return ok
}

// bareSubcommandKind refines a subcommand-less tool ("make test",
// "task lint") by its first positional arg when the arg names a known
// kind — bare tools carry no required subcommand but may still take
// one by convention.
func bareSubcommandKind(command, name string) string {
	if _, bare := bareCommandKinds[name]; !bare {
		return ""
	}
	for _, fields := range commandSegments(command) {
		if len(fields) < 2 {
			continue
		}
		if strings.TrimSuffix(filepath.Base(fields[0]), ".exe") != name {
			continue
		}
		for _, arg := range fields[1:] {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if kind := subcommandKinds[arg]; kind != "" {
				return kind
			}
		}
	}
	return ""
}

// CommandKind classifies a shell command for command memory: build,
// test, lint, run, or other. Verification commands split by their
// subcommand ("go test" is test, "go vet" is lint); bare build/test
// tools carry their own kind; a "run"/"exec" subcommand that is not a
// verification target ("npm run dev", "go run main.go") is a launch,
// not a check. Everything else is "other".
func CommandKind(command string) string {
	if name, sub, ok := matchBuildTest(command); ok {
		if sub == "" {
			if kind := bareSubcommandKind(command, name); kind != "" {
				return kind
			}
			return bareCommandKinds[name]
		}
		return subcommandKinds[sub]
	}
	for _, fields := range commandSegments(command) {
		if len(fields) >= 2 && (fields[1] == "run" || fields[1] == "exec") {
			return CommandKindRun
		}
	}
	return CommandKindOther
}
