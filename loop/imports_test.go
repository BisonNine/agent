package loop

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loop is the agent and its turn engine. It may import hook (the contract it
// implements), models and modelspec (the agent's models), toolkit and
// discovery (its tools), ctxengine with its session store (the session's
// context), accounting, clock, spawnllm (the message and response types, and
// the helpers the engine uses on them: tool-call normalisation and error
// classification; never a provider client of its own) and the standard
// library. It never imports cogmem, mcphost, mcp-go, the root agent package
// or a host. Test files may also use models/modelstest, and toolspec to
// build tools.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/loop",
		"go.bison9.com/agent/hook",
		"go.bison9.com/agent/models",
		"go.bison9.com/agent/modelspec",
		"go.bison9.com/agent/toolkit",
		"go.bison9.com/agent/discovery",
		"go.bison9.com/agent/ctxengine",
		"go.bison9.com/agent/accounting",
		"go.bison9.com/agent/clock",
		"go.bison9.com/agent/spawnllm",
		"go.bison9.com/agent/toolspec",
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
