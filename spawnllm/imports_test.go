package spawnllm

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// spawnllm sits above toolspec and below ctxengine. It may import only its
// own packages, toolspec, the shared logger, the standard library and the
// third-party modules listed here; never ctxengine or cogmem. A new provider
// SDK is added to the list when it is adopted. Test files are held to the same
// rule.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/spawnllm",
		"go.bison9.com/agent/toolspec",
		"go.bison9.com/agent/logger",
		"github.com/google/uuid",
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
