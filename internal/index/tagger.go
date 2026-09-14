package index

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxIndexFileSize caps the bytes read per file for tagging. Files
// beyond it are recorded in the files table but not tagged — the map
// shows them, their contents stay untracked. It also bounds the
// scanner's per-line buffer. Same order of magnitude as the prompt
// context-file bound.
const maxIndexFileSize = 256 * 1024

// tag is one extracted top-level declaration.
type tag struct {
	name     string
	kind     string
	line     int
	exported bool
}

// declRule maps a line to a declaration. The regex must capture the
// declared name in group 1.
type declRule struct {
	re   *regexp.Regexp
	kind string
}

// langSpec is a declaration-only grammar: enough to route the model
// to the right file, deliberately not a parser.
type langSpec struct {
	rules   []declRule
	export  func(name, line string) bool
	imports []*regexp.Regexp
	// resolve maps an import specifier to a project-relative file or
	// directory path. It returns "" when the specifier can't be
	// resolved inside the project (external deps, stdlib).
	resolve func(imp, srcDir, modulePath string, exists func(string) bool) string
}

func goExport(name, _ string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func pyExport(name, _ string) bool {
	return !strings.HasPrefix(name, "_")
}

func lineHas(marker string) func(string, string) bool {
	return func(_, line string) bool {
		return strings.Contains(line, marker)
	}
}

func anyIdent(name, _ string) bool { return true }

var langByExt = map[string]langSpec{
	".go": {
		rules: []declRule{
			{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?(\w+)`), "func"},
			{regexp.MustCompile(`^type\s+(\w+)`), "type"},
			{regexp.MustCompile(`^(?:const|var)\s+(\w+)`), "var"},
		},
		export: goExport,
		imports: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(?:\w+\s+)?"([^"]+)"`),
			regexp.MustCompile(`^import\s+"([^"]+)"`),
		},
		resolve: resolveGoImport,
	},
	".py": {
		rules: []declRule{
			{regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)`), "func"},
			{regexp.MustCompile(`^class\s+(\w+)`), "class"},
			{regexp.MustCompile(`^\s+(?:async\s+)?def\s+(\w+)`), "method"},
		},
		export: pyExport,
		imports: []*regexp.Regexp{
			regexp.MustCompile(`^from\s+([\w.]+)\s+import`),
			regexp.MustCompile(`^import\s+([\w.]+)`),
		},
		resolve: resolvePyImport,
	},
	".rs": {
		rules: []declRule{
			{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+(\w+)`), "func"},
			{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union)\s+(\w+)`), "type"},
			{regexp.MustCompile(`^\s*impl(?:<[^>]*>)?\s+(?:[\w:]+::)*(\w+)`), "impl"},
			{regexp.MustCompile(`^\s*(?:pub\s+)?mod\s+(\w+)`), "mod"},
		},
		export: lineHas("pub"),
		imports: []*regexp.Regexp{
			regexp.MustCompile(`^\s*use\s+([\w:]+)`),
			regexp.MustCompile(`^\s*(?:pub\s+)?mod\s+(\w+)\s*;`),
		},
		resolve: resolveRsImport,
	},
	".ts": jsSpec, ".tsx": jsSpec, ".js": jsSpec, ".jsx": jsSpec,
	".mjs": jsSpec, ".cjs": jsSpec,
	".java": {
		rules: []declRule{
			{regexp.MustCompile(`^\s*(?:[\w@]+\s+)*?(?:class|interface|enum|record|@interface)\s+(\w+)`), "type"},
		},
		export: lineHas("public"),
		imports: []*regexp.Regexp{
			regexp.MustCompile(`^import\s+(?:static\s+)?([\w.]+)`),
		},
		resolve: resolveJavaImport,
	},
}

// jsSpec is factored out because six extensions share it.
var jsSpec = langSpec{
	rules: []declRule{
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\*?\s+(\w+)`), "func"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:abstract\s+)?class\s+(\w+)`), "class"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type|enum)\s+(\w+)`), "type"},
		{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+(\w+)`), "var"},
		{regexp.MustCompile(`^\s+(?:(?:public|private|protected|static|async|readonly|override|abstract)\s+)*(?:get\s+|set\s+)?(\w+)\s*\(`), "method"},
	},
	export: lineHas("export"),
	imports: []*regexp.Regexp{
		regexp.MustCompile(`(?:from|import)\s+['"]([^'"]+)['"]`),
		regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`),
	},
	resolve: resolveJSImport,
}

// jsKeywords keeps the method rule from matching control-flow lines.
var jsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "return": true,
	"switch": true, "catch": true, "function": true, "new": true,
	"constructor": true, "typeof": true, "await": true, "else": true,
	"do": true, "case": true, "throw": true, "yield": true,
}

// tagFile extracts declarations and resolved references from one file.
// exists reports whether a project-relative path is a known file or a
// directory containing known files; modulePath is the go.mod module
// path when present.
func tagFile(root, relPath, modulePath string, exists func(string) bool) ([]tag, []string, error) {
	spec, ok := langByExt[filepath.Ext(relPath)]
	if !ok {
		return nil, nil, nil
	}
	f, err := os.Open(filepath.Join(root, relPath))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	// Oversized files are recorded in the files table but not tagged —
	// the map still shows them.
	if st, err := f.Stat(); err != nil || st.Size() > maxIndexFileSize {
		return nil, nil, nil
	}

	// Sniff the first block for NUL bytes — binary files get recorded
	// in the files table but never tagged.
	br := bufio.NewReader(f)
	if head, _ := br.Peek(512); isProbablyBinary(head) {
		return nil, nil, nil
	}

	var tags []tag
	refSet := map[string]bool{}
	srcDir := filepath.Dir(relPath)

	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64*1024), maxIndexFileSize)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}
		for _, rule := range spec.rules {
			m := rule.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			if rule.kind == "method" && jsKeywords[name] {
				continue
			}
			tags = append(tags, tag{
				name:     name,
				kind:     rule.kind,
				line:     lineNo,
				exported: spec.export(name, line),
			})
			break
		}
		for _, re := range spec.imports {
			if m := re.FindStringSubmatch(line); m != nil {
				if dst := spec.resolve(m[1], srcDir, modulePath, exists); dst != "" {
					refSet[dst] = true
				}
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return tags, nil, nil // Truncated/binary tail: keep what we got.
	}
	refs := make([]string, 0, len(refSet))
	for r := range refSet {
		refs = append(refs, r)
	}
	return tags, refs, nil
}

// isProbablyBinary sniffs the first block for NUL bytes.
func isProbablyBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}

// resolveGoImport maps a Go import path to the project-relative
// package directory when it lives under the module path.
func resolveGoImport(imp, _, modulePath string, exists func(string) bool) string {
	if modulePath == "" || !strings.HasPrefix(imp, modulePath) {
		return ""
	}
	dir := strings.TrimPrefix(imp, modulePath)
	dir = strings.TrimPrefix(dir, "/")
	if dir == "" {
		return ""
	}
	// A package dir counts when indexed files sit under it — exists
	// covers directories as well as files.
	if exists(dir) {
		return dir
	}
	return ""
}

// probeExt returns the first candidate that exists, or "".
func probeExt(base string, exists func(string) bool, exts ...string) string {
	for _, ext := range exts {
		if exists(base + ext) {
			return base + ext
		}
	}
	return ""
}

func resolveJSImport(imp, srcDir, _ string, exists func(string) bool) string {
	if !strings.HasPrefix(imp, ".") {
		return "" // Package imports aren't project files.
	}
	base := filepath.Clean(filepath.Join(srcDir, imp))
	base = filepath.ToSlash(base)
	if exists(base) {
		return base
	}
	if p := probeExt(base, exists, ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"); p != "" {
		return p
	}
	return probeExt(base+"/index", exists, ".ts", ".tsx", ".js", ".jsx")
}

func resolvePyImport(imp, _, _ string, exists func(string) bool) string {
	base := strings.ReplaceAll(imp, ".", "/")
	if p := probeExt(base, exists, ".py"); p != "" {
		return p
	}
	if exists(base + "/__init__.py") {
		return base + "/__init__.py"
	}
	return ""
}

func resolveRsImport(imp, srcDir, _ string, exists func(string) bool) string {
	imp = strings.TrimPrefix(imp, "crate::")
	imp = strings.TrimPrefix(imp, "self::")
	parts := strings.Split(imp, "::")
	if len(parts) == 0 {
		return ""
	}
	// Rust paths resolve relative to the crate src dir or the current
	// file's dir for `mod` declarations; probe both shapes.
	for _, base := range []string{
		filepath.ToSlash(filepath.Join("src", filepath.Join(parts[:len(parts)-1]...))) + "/" + parts[len(parts)-1],
		filepath.ToSlash(filepath.Join(srcDir, parts[len(parts)-1])),
	} {
		if p := probeExt(base, exists, ".rs"); p != "" {
			return p
		}
		if exists(base + "/mod.rs") {
			return base + "/mod.rs"
		}
	}
	return ""
}

func resolveJavaImport(imp, _, _ string, exists func(string) bool) string {
	p := strings.ReplaceAll(imp, ".", "/") + ".java"
	if exists(p) {
		return p
	}
	// Maven/Gradle layout.
	if idx := strings.Index(p, "/"); idx >= 0 {
		for _, root := range []string{"src/main/java/", "src/test/java/"} {
			if exists(root + p) {
				return root + p
			}
		}
	}
	return ""
}
