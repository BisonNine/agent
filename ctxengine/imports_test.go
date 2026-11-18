package ctxengine

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ctxengine sits above spawnllm and toolspec. Of the SDK's packages it may
// import only its own, spawnllm, toolspec and the shared logger; never cogmem.
// Test files are held to the same rule.
func TestImportsRespectLayering(t *testing.T) {
	const module = "go.bison9.com/agent/"
	allowed := []string{
		module + "ctxengine",
		module + "spawnllm",
		module + "toolspec",
		module + "logger",
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
			if !strings.HasPrefix(name, module) {
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
