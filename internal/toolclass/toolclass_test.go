package toolclass

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBashRedirectTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		want    []string
	}{
		{name: "simple redirect", command: "echo hi > out.txt", want: []string{"out.txt"}},
		{name: "append", command: "echo hi >> out.txt", want: []string{"out.txt"}},
		{name: "no space", command: "echo hi >out.txt", want: []string{"out.txt"}},
		{name: "quoted target", command: "echo hi > 'my file.txt'", want: []string{"my file.txt"}},
		{name: "double-quoted target", command: `echo hi > "my file.txt"`, want: []string{"my file.txt"}},
		{name: "heredoc then redirect", command: "cat <<'EOF' > out.txt\ncontent\nEOF", want: []string{"out.txt"}},
		{name: "redirect then heredoc", command: "cat > 'out.txt' <<'EOF'\ncontent\nEOF", want: []string{"out.txt"}},
		{name: "quoted gt is not a redirect", command: "echo 'a > b'", want: nil},
		{name: "double-quoted gt is not a redirect", command: `echo "a > b"`, want: nil},
		{name: "dev null is not a write", command: "cmd > /dev/null", want: nil},
		{name: "fd dup is not a write", command: "cmd >&1", want: nil},
		{name: "fd close is not a write", command: "cmd >&-", want: nil},
		{name: "stderr redirect", command: "cmd 2>err.log", want: []string{"err.log"}},
		{name: "stderr fd dup", command: "cmd 2>&1", want: nil},
		{name: "combined redirect to file", command: "cmd >&out.txt", want: []string{"out.txt"}},
		{name: "pipeline terminator", command: "cmd > out | grep x", want: []string{"out"}},
		{name: "sequence terminator", command: "cmd > out; ls", want: []string{"out"}},
		{name: "multiple redirects", command: "cmd > a.txt 2> b.txt", want: []string{"a.txt", "b.txt"}},
		{name: "no redirect", command: "sed -i 's/a/b/' f.go", want: nil},
		{name: "test comparison is not a redirect", command: "[[ $a > b.go ]]", want: nil},
		{name: "test comparison with real redirect", command: "[[ $a > b ]] && cmd > out.txt", want: []string{"out.txt"}},
		{name: "arithmetic comparison is not a redirect", command: "echo $((a > b))", want: nil},
		{name: "arithmetic with real redirect", command: "echo $((a > b)) > out.txt", want: []string{"out.txt"}},
		{name: "heredoc body gt is not a redirect", command: "cat <<'EOF'\n> line\nEOF", want: nil},
		{name: "unquoted heredoc body", command: "cat <<EOF\n> line\nEOF", want: nil},
		{name: "heredoc body with real redirect", command: "cat <<'EOF' > out.txt\n> line\nEOF", want: []string{"out.txt"}},
		{name: "dash-strip heredoc body", command: "cat <<-'EOF'\n\t> line\n\tEOF", want: nil},
		{name: "unterminated heredoc masks rest", command: "cat <<'EOF'\n> never\n> stops", want: nil},
		{name: "tilde target not concrete", command: "cmd > ~/out", want: nil},
		{name: "variable target not concrete", command: "cmd > $OUT", want: nil},
		{name: "glob target not concrete", command: "cmd > *.log", want: nil},
		{name: "substitution target not concrete", command: "cmd > $(gen)", want: nil},
		{name: "escaped-space target", command: `cmd > my\ file.txt`, want: []string{"my file.txt"}},
		{name: "escaped backslash in target", command: `cmd > a\\b`, want: []string{`a\b`}},
		{name: "bare >& yields nothing", command: `cmd >&`, want: nil},
		{name: "bare >& before separator", command: `cmd >& ; ls`, want: nil},
		{name: "escaped quote inside double quotes", command: `echo "a \"b" > out`, want: []string{"out"}},
		{name: "escaped quote is not a phantom span", command: `echo "a \"b > out"`, want: nil},
		{name: "ansi-c quoted gt is not a redirect", command: `echo $'a > b'`, want: nil},
		{name: "ansi-c quote before real redirect", command: `echo $'x' > out`, want: []string{"out"}},
		{name: "herestring is not a heredoc", command: "cat <<< foo\n> out.txt", want: []string{"out.txt"}},
		{name: "quoted herestring is not a heredoc", command: "cat <<< \"foo\"\n> out.txt", want: []string{"out.txt"}},
		{name: "newline before delimiter is not a heredoc", command: "cat <<\nEOF\n> out.txt", want: []string{"out.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, BashRedirectTargets(tc.command))
		})
	}
}

func TestBashEditTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		want    []string
	}{
		// Redirects come through the shared scan.
		{name: "redirect", command: "go build > out.log", want: []string{"out.log"}},
		{name: "redirect and sed", command: "sed -i 's/a/b/' f.go > log", want: []string{"log", "f.go"}},

		// sed -i forms.
		{name: "sed -i", command: "sed -i 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed -i bundled", command: "sed -ni 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed --in-place", command: "sed --in-place 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed -i suffix attached", command: "sed -i.bak 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed -i BSD empty suffix", command: "sed -i '' 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed -i BSD named suffix", command: "sed -i .bak 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed -e script flag", command: "sed -i -e 's/a/b/' -e 's/c/d/' f.go", want: []string{"f.go"}},
		{name: "sed multiple files", command: "sed -i 's/a/b/' a.go b.go", want: []string{"a.go", "b.go"}},
		{name: "sed no -i is stream-only", command: "sed -n 's/a/b/p' f.go", want: nil},
		{name: "sed -i script only", command: "sed -i 's/a/b/'", want: nil},
		{name: "sed -i after pipe", command: "grep -l foo *.go | xargs cat; sed -i 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed in quoted span is not a command", command: "echo 'sed -i s/a/b/ f.go'", want: nil},
		{name: "sed -i with redirect", command: "sed -i 's/a/b/' f.go 2> err.log", want: []string{"err.log", "f.go"}},

		// perl -i forms.
		{name: "perl -pi -e", command: "perl -pi -e 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "perl -i -e", command: "perl -i -e 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "perl -pi script operand", command: "perl -pi 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "perl -pi.bak", command: "perl -pi.bak -e 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "perl no -i", command: "perl -ne 'print' f.go", want: nil},

		// tee forms.
		{name: "tee", command: "cmd | tee out.log", want: []string{"out.log"}},
		{name: "tee -a", command: "cmd | tee -a out.log", want: []string{"out.log"}},
		{name: "tee multiple", command: "cmd | tee a.log b.log", want: []string{"a.log", "b.log"}},
		{name: "tee with redirect", command: "cmd | tee out.log 2> err.log", want: []string{"err.log", "out.log"}},

		// Mutations that author no content carry no evidence target.
		{name: "rm", command: "rm -rf dist", want: nil},
		{name: "cp", command: "cp a.go b.go", want: nil},
		{name: "mv", command: "mv a.go b.go", want: nil},
		{name: "chmod", command: "chmod +x f.sh", want: nil},
		{name: "mkdir", command: "mkdir -p out", want: nil},
		{name: "git commit", command: "git commit -m x", want: nil},
		{name: "patch diff names its own targets", command: "patch -p1 < fix.diff", want: nil},

		// Non-concrete targets are dropped.
		{name: "redirect to variable", command: "cmd > $OUT", want: nil},
		{name: "sed -i glob", command: "sed -i 's/a/b/' *.go", want: nil},
		{name: "tee variable", command: "cmd | tee $LOG", want: nil},
		{name: "tee behind sudo", command: "cmd | sudo tee /etc/app.conf", want: []string{"/etc/app.conf"}},
		{name: "tee behind pipe-stderr", command: "cmd |& tee out.log", want: []string{"out.log"}},
		{name: "sed behind env assignment", command: "env LC_ALL=C sed -i 's/a/b/' f.go", want: []string{"f.go"}},
		{name: "sed behind env plus flag arg", command: "env LC_ALL=C nice -n 5 sed -i 's/a/b/' f.go", want: nil}, // Flag arity after a prefix stays un-gated.
		{name: "sed behind time", command: "time sed -i 's/a/b/' f.go", want: []string{"f.go"}},

		// Comments manufacture no targets — a separator or `>`
		// inside one is literal text, and comment words never
		// become operands.
		{name: "comment hides writer", command: "go build # TODO: try sed -i 's/x/y/' main.go; tee log", want: nil},
		{name: "comment hides redirect", command: "cmd #> f", want: nil},
		{name: "comment words not operands", command: "sed -i 's/a/b/' f.go # then ; tee log", want: []string{"f.go"}},
		{name: "redirect before comment", command: "cmd > f # and also ; tee g", want: []string{"f"}},
		{name: "whole-line comment", command: "# sed -i 's/a/b/' f.go", want: nil},
		{name: "mid-word hash not comment", command: "cmd a#b; tee g", want: []string{"g"}},
		{name: "quote-glued hash not comment", command: `cmd "x"#; tee g`, want: []string{"g"}},
		{name: "quoted hash not comment", command: "cmd '#still runs' | tee g", want: []string{"g"}},

		// Dedup across routes.
		{name: "same file twice", command: "sed -i 's/a/b/' f.go && cmd > f.go", want: []string{"f.go"}},
		{name: "same file redirect and tee", command: "cmd | tee log && cmd > log", want: []string{"log"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, BashEditTargets(tc.command))
		})
	}
}

func TestCommandKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		want    string
	}{
		{"go test ./...", CommandKindTest},
		{"go build .", CommandKindBuild},
		{"go vet ./...", CommandKindLint},
		{"npm run test", CommandKindTest},
		{"npm run dev", CommandKindRun},
		{"go run main.go", CommandKindRun},
		{"make", CommandKindBuild},
		{"make test", CommandKindTest},
		{"make -j4 lint", CommandKindLint},
		{"task -t Taskfile lint", CommandKindLint},
		{"make -C test build", CommandKindBuild},
		{"pytest -x", CommandKindTest},
		{"env FOO=1 go test ./...", CommandKindTest},
		{"ls -la", CommandKindOther},
		{"cd x && go test", CommandKindTest},
		{"npm ci", CommandKindOther},
		{"vitest run", CommandKindTest},
		{"vitest related src/a.ts", CommandKindTest},
		{"jest", CommandKindTest},
		{"npx vitest run", CommandKindTest},
		{"pnpm dlx vitest run", CommandKindTest},
		{"npm exec tsc", CommandKindLint},
		{"npm start", CommandKindRun},
		{"yarn dev", CommandKindRun},
		{"sudo -n go test", CommandKindTest},
		{"nice -n 5 go test", CommandKindTest},
		{"timeout 60 go test", CommandKindTest},
		{"timeout -k 5 60 go test", CommandKindTest},
		{"stdbuf -oL go test", CommandKindTest},
		{"watch -n 2 make test", CommandKindTest},
		{"xargs go test", CommandKindTest},
		{"mvnw test", CommandKindTest},
		{"./mvnw verify", CommandKindTest},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, CommandKind(tc.command), tc.command)
	}
}

func TestCommandKind_RoundFiveEdges(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"time -p is a flag not an arg", "time -p make", "build"},
		{"bun x passthrough", "bun x vitest run", "test"},
		{"npm exec flags", "npm exec --yes -- vitest run", "test"},
		{"npm exec bare tool", "npm exec -- tsc --noEmit", "lint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommandKind(tt.command); got != tt.want {
				t.Fatalf("CommandKind(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}
