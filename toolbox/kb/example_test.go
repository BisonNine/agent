/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"
)

// workspaceResolver is an example policy, the one the first host uses. It
// is not part of the package: a host writes its own.
//
//   - Own bundles are the directories directly under a confined folder (the
//     agent's okf/ folder, say), named without "/". They are writable, and a
//     put creates a missing one; Confine keeps a link under the folder from
//     leading out of it.
//   - Shared bundles are an explicit list, addressed as "shared/<name>". A
//     put never creates them.
type workspaceResolver struct {
	folder string
	shared []sharedBundle
}

type sharedBundle struct {
	name     string
	root     string
	writable bool
}

const sharedPrefix = "shared/"

func (w workspaceResolver) Resolve(_ context.Context, name string) (kb.Bundle, error) {
	if rest, ok := strings.CutPrefix(name, sharedPrefix); ok {
		for _, s := range w.shared {
			if s.name == rest {
				return kb.Bundle{Name: name, Root: s.root, Writable: s.writable}, nil
			}
		}
		return kb.Bundle{}, kb.ErrUnknownKB
	}
	if !ownName(name) {
		return kb.Bundle{}, kb.ErrUnknownKB
	}
	return kb.Bundle{
		Name: name, Root: filepath.Join(w.folder, name), Confine: w.folder,
		Writable: true, Create: true,
	}, nil
}

func (w workspaceResolver) Bundles(context.Context) ([]kb.Info, error) {
	var out []kb.Info
	entries, err := os.ReadDir(w.folder)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() && ownName(e.Name()) {
			out = append(out, kb.Info{Name: e.Name(), Writable: true})
		}
	}
	for _, s := range w.shared {
		out = append(out, kb.Info{Name: sharedPrefix + s.name, Writable: s.writable})
	}
	return out, nil
}

// ownName reports whether name can be an own bundle: one path segment, not
// hidden, no separators or control characters.
func ownName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, `/\`) &&
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func Example() {
	dir, err := os.MkdirTemp("", "kb-example")
	if err != nil {
		panic(err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			panic(rmErr)
		}
	}()
	ccRoot := filepath.Join(dir, "cc2022")
	if mkErr := os.MkdirAll(filepath.Join(ccRoot, "SFR"), 0o700); mkErr != nil {
		panic(mkErr)
	}
	if wrErr := os.WriteFile(filepath.Join(ccRoot, "SFR", "FCS_COP.1.md"),
		[]byte("---\ntype: SFR\ntitle: Cryptographic operation\n---\nThe TSF shall perform encryption.\n"), 0o600); wrErr != nil {
		panic(wrErr)
	}

	store, err := kb.New(kb.Options{})
	if err != nil {
		panic(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			panic(closeErr)
		}
	}()
	alice := workspaceResolver{
		folder: filepath.Join(dir, "alice", "okf"),
		shared: []sharedBundle{{name: "cc2022", root: ccRoot}},
	}
	ctx := context.Background()

	res, err := store.Put(ctx, alice, "audit-methods", "guidance/crypto-review", kb.Frontmatter{
		"type": "Evaluation Guidance", "title": "Review cryptographic claims", "tags": []string{"cryptography"},
		kb.IncludeKey: []string{},
	}, "Record the evidence for each claimed algorithm and key size.\n")
	if err != nil {
		panic(err)
	}
	fmt.Println("put", res.KB, res.ID, "created:", res.Created)

	rec, err := store.Get(ctx, alice, "shared/cc2022", "SFR/FCS_COP.1")
	if err != nil {
		panic(err)
	}
	fmt.Println("get", rec.ID, "-", rec.Frontmatter.Title())

	hits, err := store.Search(ctx, alice, "audit-methods", "key size", 0)
	if err != nil {
		panic(err)
	}
	fmt.Println("search", hits[0].ID)

	_, err = store.Put(ctx, alice, "shared/cc2022", "SFR/x", kb.Frontmatter{"type": "SFR"}, "")
	fmt.Println(err)

	bundles, err := alice.Bundles(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(bundles)
	// Output:
	// put audit-methods guidance/crypto-review created: true
	// get SFR/FCS_COP.1 - Cryptographic operation
	// search guidance/crypto-review
	// knowledge base is read-only: "shared/cc2022"
	// [{audit-methods true} {shared/cc2022 false}]
}

func TestWorkspaceResolver(t *testing.T) {
	dir := t.TempDir()
	store, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	bob := workspaceResolver{
		folder: filepath.Join(dir, "bob", "okf"),
		shared: []sharedBundle{
			{name: "team", root: filepath.Join(dir, "team"), writable: true},
			{name: "gone", root: filepath.Join(dir, "gone")},
		},
	}
	ctx := context.Background()
	if bundles, listErr := bob.Bundles(ctx); listErr != nil || len(bundles) != 2 {
		t.Fatalf("Bundles before any own bundle = %v, %v", bundles, listErr)
	}
	if _, putErr := store.Put(ctx, bob, "notes", "a", kb.Frontmatter{"type": "Note"}, ""); putErr != nil {
		t.Fatal(putErr)
	}
	if mkErr := os.MkdirAll(filepath.Join(bob.folder, ".git"), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	bundles, err := bob.Bundles(ctx)
	want := []kb.Info{{Name: "notes", Writable: true}, {Name: "shared/team", Writable: true}, {Name: "shared/gone"}}
	if err != nil || !reflect.DeepEqual(bundles, want) {
		t.Fatalf("Bundles = %v, %v; want %v", bundles, err, want)
	}
	// A shared bundle is never created by a put.
	if _, err := store.Put(ctx, bob, "shared/team", "a", kb.Frontmatter{"type": "Note"}, ""); !errors.Is(err, kb.ErrUnknownKB) {
		t.Fatalf("Put to a missing shared bundle = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "team"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, bob, "shared/team", "a", kb.Frontmatter{"type": "Note"}, ""); err != nil {
		t.Fatalf("Put to a writable shared bundle = %v", err)
	}
	for _, name := range []string{"shared/nope", "a/b", "../x", ".hidden", "", "team"} {
		_, err := store.Get(ctx, bob, name, "a")
		if name == "team" {
			// An own bundle that does not exist yet reads as empty.
			if !errors.Is(err, kb.ErrNotFound) {
				t.Errorf("Get from %q = %v", name, err)
			}
			continue
		}
		if !errors.Is(err, kb.ErrUnknownKB) {
			t.Errorf("Get from %q = %v, want ErrUnknownKB", name, err)
		}
	}
	// An own bundle linked out of the folder is refused.
	if err := os.Symlink(filepath.Join(dir, "team"), filepath.Join(bob.folder, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, bob, "linked", "a"); err == nil {
		t.Fatal("Get followed a link out of the folder")
	}
}
