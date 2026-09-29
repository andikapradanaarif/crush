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
