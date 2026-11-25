/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package tools

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// tools imports only kb, toolspec and the standard library, so it can use
// nothing a host's own code cannot. Test files are held to the same rule.
func TestImportsRespectLayering(t *testing.T) {
	allowed := map[string]bool{
		"go.bison9.com/agent/toolbox/kb": true,
		"go.bison9.com/agent/toolspec":   true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			// A standard-library path has no dot in its first element.
			if first, _, _ := strings.Cut(name, "/"); !strings.Contains(first, ".") {
				continue
			}
			if !allowed[name] {
				t.Errorf("%s imports %s", e.Name(), name)
			}
		}
	}
}
