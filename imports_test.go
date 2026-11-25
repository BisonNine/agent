/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The root package is a front door, not a workshop: besides tests it has
// only agent.go (the interface and New), options.go and aliases.go, starts
// no goroutine, and imports only the SDK packages it re-exports or builds
// options over, the standard library's context, log/slog and time, and (in
// tests) what the tests need.
func TestRootIsAFrontDoor(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowedFiles := []string{"agent.go", "options.go", "aliases.go"}
	allowedImports := []string{
		"context", "log/slog", "time",
		"go.bison9.com/agent/loop",
		"go.bison9.com/agent/hook",
		"go.bison9.com/agent/models",
		"go.bison9.com/agent/toolkit",
		"go.bison9.com/agent/clock",
		"go.bison9.com/agent/ctxengine",
		"go.bison9.com/agent/ctxengine/session",
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !slices.Contains(allowedFiles, name) {
			t.Errorf("%s: the root package holds only %v and their tests", name, allowedFiles)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(allowedImports, path) {
				t.Errorf("%s imports %s", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s:%d starts a goroutine", name, fset.Position(g.Pos()).Line)
			}
			return true
		})
	}
}
