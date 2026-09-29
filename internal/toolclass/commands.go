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
	// "npm ci" is a clean-install, not a test run — it is not a
	// subcommand here. "npm start" is shorthand for "npm run start".
	"npm":           {"test", "start"},
	"pnpm":          {"test", "build", "lint", "start", "dev"},
	"yarn":          {"test", "build", "lint", "start", "dev"},
	"bun":           {"test", "build", "start", "dev"},
	"deno":          {"test", "check", "lint"},
	"dotnet":        {"build", "test"},
	"mvn":           {"compile", "test", "verify", "package"},
	"mvnw":          {"compile", "test", "verify", "package"},
	"gradle":        {"build", "test", "check"},
	"gradlew":       {"build", "test", "check"},
	"cmake":         {"--build"},
	"vitest":        {"run", "related", "typecheck"},
	"pytest":        {},
	"tsc":           {},
	"make":          {},
	"task":          {},
	"just":          {},
	"ctest":         {},
	"golangci-lint": {},
	"staticcheck":   {},
	"jest":          {},
	"mocha":         {},
	"ava":           {},
	"tap":           {},
}

// runSubIsTest lists test runners whose "run" subcommand IS the test
// invocation ("vitest run"), unlike "npm run dev" which launches.
var runSubIsTest = map[string]bool{
	"vitest": true, "jest": true, "mocha": true, "ava": true, "tap": true,
}

// packageRunners take "dlx"/"exec" passthrough subcommands that precede
// the real tool name: "pnpm dlx vitest", "npm exec tsc".
var packageRunners = map[string]bool{
	"npm": true, "pnpm": true, "yarn": true, "bun": true,
}

// buildTestRunTargets lists script names accepted after a "run"
// subcommand (e.g. "npm run build").
var buildTestRunTargets = []string{
	"build", "test", "lint", "check", "typecheck", "type-check", "tsc", "ci",
}

// commandWrappers are leading words that wrap the real command.
var commandWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "time": true,
	"nice": true, "nohup": true, "command": true, "exec": true,
	"npx": true, "bunx": true, "pnpx": true,
	"timeout": true, "stdbuf": true, "watch": true, "xargs": true,
}

// wrapperFlagArgs are wrapper flags that consume a following value
// argument (e.g. "env -u NAME", "nice -n 5", "sudo -u root"). Keyed by
// wrapper then flag name without leading dashes — flags are per-tool,
// so sudo's bare "-n" never eats the command like nice's "-n 5" must.
var wrapperFlagArgs = map[string]map[string]bool{
	"sudo":    {"u": true, "g": true, "h": true, "p": true, "D": true, "R": true, "T": true, "t": true, "U": true, "C": true, "chdir": true},
	"doas":    {"u": true},
	"env":     {"u": true, "unset": true, "C": true, "chdir": true, "S": true, "split-string": true, "P": true, "alternate-argv": true},
	"nice":    {"n": true, "adjustment": true},
	"nohup":   {},
	"command": {},
	"exec":    {},
	"time":    {"o": true, "f": true, "a": true, "p": true, "output": true, "format": true, "append": true},
	"npx":     {"p": true, "package": true, "c": true, "call": true},
	"bunx":    {"b": true, "bun": true, "p": true, "package": true},
	"pnpx":    {"p": true, "package": true},
	"timeout": {"k": true, "kill-after": true, "s": true, "signal": true},
	"stdbuf":  {"i": true, "input": true, "o": true, "output": true, "e": true, "error": true},
	"watch":   {"n": true, "interval": true},
	"xargs":   {"I": true, "replace": true, "n": true, "max-args": true, "P": true, "max-procs": true, "s": true, "max-chars": true, "d": true, "delimiter": true, "L": true, "max-lines": true},
}

// wrapperPositionalArgs counts positional args a wrapper consumes
// before the wrapped command starts — "timeout 60 go test" carries the
// duration as its first positional, so it is skipped, while "xargs go
// test" runs the positional itself.
var wrapperPositionalArgs = map[string]int{
	"timeout": 1,
}

// bareCommandKinds are the kinds for tools that need no subcommand —
// the tool name itself carries the meaning.
var bareCommandKinds = map[string]string{
	"pytest":        CommandKindTest,
	"ctest":         CommandKindTest,
	"jest":          CommandKindTest,
	"mocha":         CommandKindTest,
	"ava":           CommandKindTest,
	"tap":           CommandKindTest,
	"tsc":           CommandKindLint,
	"golangci-lint": CommandKindLint,
	"staticcheck":   CommandKindLint,
	"make":          CommandKindBuild,
	"task":          CommandKindBuild,
	"just":          CommandKindBuild,
}

// bareToolFlagArgs are bare-tool flags that consume a following value —
// "make -C dir test" skips dir, "task -t Taskfile lint" skips
// Taskfile — so a flag value never masquerades as the positional
// subcommand that sets the kind.
var bareToolFlagArgs = map[string]map[string]bool{
	"make": {"C": true, "f": true, "I": true, "j": true, "l": true, "o": true, "O": true, "W": true,
		"directory": true, "file": true, "makefile": true, "include-dir": true, "jobs": true,
		"old-file": true, "new-file": true, "what-if": true, "output-sync": true},
	"task": {"t": true, "taskfile": true, "d": true, "dir": true, "c": true, "concurrency": true,
		"o": true, "output": true, "sort": true},
	"just": {"f": true, "justfile": true, "d": true, "working-directory": true, "set": true,
		"shell": true, "shell-arg": true, "chooser": true, "dotenv-path": true, "dotenv-filename": true},
}

// subcommandKinds map a recognized build/test subcommand to its kind.
// "start"/"dev"/"serve" are script launches, not checks.
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
	"related":    CommandKindTest,
	"start":      CommandKindRun,
	"dev":        CommandKindRun,
	"serve":      CommandKindRun,
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
		// Skip leading env assignments, command wrappers, wrapper
		// flags, and wrapper-consumed positionals so "CGO_ENABLED=0
		// go test", "env -i go test", "nice -n 5 make", or
		// "timeout 60 go test" still classify.
		wrapper := ""
		skipPositional := 0
	fieldsLoop:
		for len(fields) > 0 {
			f := fields[0]
			switch {
			case isEnvAssignment(f):
				fields = fields[1:]
			case commandWrappers[f]:
				wrapper = f
				skipPositional += wrapperPositionalArgs[f]
				fields = fields[1:]
			case wrapper != "" && strings.HasPrefix(f, "-"):
				fields = fields[1:]
				if wrapperFlagArgs[wrapper][strings.TrimLeft(f, "-")] && len(fields) > 0 {
					fields = fields[1:]
				}
			case wrapper != "" && skipPositional > 0:
				skipPositional--
				fields = fields[1:]
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
		// Package-runner passthroughs precede the real tool name:
		// "pnpm dlx vitest run" classifies as "vitest run".
		if packageRunners[name] && len(fields) >= 3 && (fields[1] == "dlx" || fields[1] == "exec") {
			return matchBuildTest(strings.Join(fields[2:], " "))
		}
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
			// For a test runner "run" is the test invocation
			// ("vitest run"), not a script launch.
			if runSubIsTest[name] {
				return name, "test", true
			}
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
		args := fields[1:]
		flagArgs := bareToolFlagArgs[name]
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if strings.HasPrefix(arg, "-") {
				// A value-taking flag consumes the next arg — it is
				// a value, never the positional that sets the kind.
				if flagArgs[strings.TrimLeft(arg, "-")] {
					i++
				}
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
