package hook

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hook is the leaf the agent loop is built on: it imports only the standard
// library, toolkit (tool results), spawnllm's protocoltypes (the message,
// response and tool-definition types, without the provider clients) and
// accounting (Scope.Identity). It never imports the loop, models, ctxengine,
// cogmem, a host, or a third-party module. Test files are held to the same
// rule.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/hook",
		"go.bison9.com/agent/toolkit",
		"go.bison9.com/agent/spawnllm/protocoltypes",
		"go.bison9.com/agent/accounting",
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range parsed.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			// A standard-library path has no dot in its first element.
			if first, _, _ := strings.Cut(name, "/"); !strings.Contains(first, ".") {
				continue
			}
			ok := false
			for _, a := range allowed {
				if name == a || strings.HasPrefix(name, a+"/") {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s imports %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
