/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// kb imports only fsmode, the standard library, yaml.v3, jsonschema and
// SQLite; never toolspec (the tools package publishes it) or a host. Test
// files are held to the same rule. The tools subpackage has its own guard.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/fsmode",
		"go.bison9.com/agent/toolbox/kb",
		"github.com/santhosh-tekuri/jsonschema/v6",
		"gopkg.in/yaml.v3",
		"modernc.org/sqlite",
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
			// Subpackages of the allowed modules are allowed, but not kb's
			// own: the tools subpackage depends on kb, never the reverse.
			ok := false
			for _, a := range allowed {
				if name == a || strings.HasPrefix(name, a+"/") && a != "go.bison9.com/agent/toolbox/kb" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s imports %s", e.Name(), name)
			}
		}
	}
}
