package agenttest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// agenttest drives any agent.Agent: it may import the root package, hook,
// models with modelstest, modelspec, toolkit, toolspec, the session store,
// spawnllm's types and the standard library, never loop (the suite must not
// know the implementation it tests) or a host.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/agenttest",
		"go.bison9.com/agent/hook",
		"go.bison9.com/agent/models",
		"go.bison9.com/agent/modelspec",
		"go.bison9.com/agent/toolkit",
		"go.bison9.com/agent/toolspec",
		"go.bison9.com/agent/ctxengine/session",
		"go.bison9.com/agent/spawnllm",
	}
	root := "go.bison9.com/agent"
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
			ok := name == root
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
