package translator_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// playFlagsPath is the checked-in list of `vllm-vcr play` flags this translator
// can emit. hack/check-vllm-vcr-upstream.sh compares it against the pinned
// simulator release, so it must describe the code exactly.
const playFlagsPath = "../../hack/vllm-vcr-play-flags.txt"

// playFlagFunctions are the functions that build the simulator invocation. Every
// `--flag` literal inside them is a play flag; literals elsewhere in the package
// belong to the injected Python frontend.
var playFlagFunctions = []string{"backendInvocation", "traceArgs"}

// TestPlayFlagSurfaceMatchesDeclaration keeps the checked-in play-flag list and
// the code that emits it in step. The list is what the upstream drift check
// validates, so a flag added to the invocation without registering it here would
// silently escape the check.
func TestPlayFlagSurfaceMatchesDeclaration(t *testing.T) {
	declared, err := readPlayFlags(playFlagsPath)
	if err != nil {
		t.Fatalf("read %s: %v", playFlagsPath, err)
	}
	emitted, err := emittedPlayFlags("frontend.go")
	if err != nil {
		t.Fatalf("scan frontend.go: %v", err)
	}

	if missing := difference(emitted, declared); len(missing) > 0 {
		t.Errorf("the translator emits play flags missing from %s: %v", playFlagsPath, missing)
	}
	if stale := difference(declared, emitted); len(stale) > 0 {
		t.Errorf("%s lists play flags the translator never emits: %v", playFlagsPath, stale)
	}
}

// TestPlayFlagsAreLongFormNames guards the format the upstream comparison relies
// on: one bare `--name` per entry, no `=value`, no negation and no short form.
func TestPlayFlagsAreLongFormNames(t *testing.T) {
	declared, err := readPlayFlags(playFlagsPath)
	if err != nil {
		t.Fatalf("read %s: %v", playFlagsPath, err)
	}
	for _, flag := range declared {
		switch {
		case !strings.HasPrefix(flag, "--"):
			t.Errorf("%s: %q is not a long-form flag", playFlagsPath, flag)
		case strings.Contains(flag, "="):
			t.Errorf("%s: %q embeds a value; list bare flag names", playFlagsPath, flag)
		case len(flag) < 3:
			t.Errorf("%s: %q is not a flag name", playFlagsPath, flag)
		}
	}
}

func readPlayFlags(path string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var flags []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		flags = append(flags, line)
	}
	return flags, nil
}

// emittedPlayFlags parses a Go source file and returns the sorted, deduplicated
// flag literals inside playFlagFunctions.
func emittedPlayFlags(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	want := make(map[string]struct{}, len(playFlagFunctions))
	for _, name := range playFlagFunctions {
		want[name] = struct{}{}
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, interesting := want[fn.Name.Name]; !interesting {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(value, "--") {
				return true
			}
			if name, _, found := strings.Cut(value, "="); found {
				value = name
			}
			seen[value] = struct{}{}
			return true
		})
	}
	flags := make([]string, 0, len(seen))
	for flag := range seen {
		flags = append(flags, flag)
	}
	slices.Sort(flags)
	return flags, nil
}

// difference returns the members of a that are absent from b.
func difference(a, b []string) []string {
	var out []string
	for _, value := range a {
		if !slices.Contains(b, value) {
			out = append(out, value)
		}
	}
	return out
}
