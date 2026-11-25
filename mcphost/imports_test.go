/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mcphost serves toolkit registries over MCP: it imports only toolkit,
// toolspec, hook, the standard library and mcp-go. It is the only SDK
// package that imports mcp-go, and nothing else in the SDK imports it, so a
// host with no CLI model never links mcp-go. It never imports a host. Test
// files are held to the same rule.
func TestImportsRespectLayering(t *testing.T) {
	allowed := []string{
		"go.bison9.com/agent/mcphost",
		"go.bison9.com/agent/toolkit",
		"go.bison9.com/agent/toolspec",
		"go.bison9.com/agent/hook",
		"github.com/mark3labs/mcp-go",
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
