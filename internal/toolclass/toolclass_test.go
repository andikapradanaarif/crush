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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, BashRedirectTargets(tc.command))
		})
	}
}
