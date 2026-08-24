// Package arch contains architecture conformance tests. The module
// boundaries of the modular monolith are enforced here so that a violating
// import fails the build in CI instead of relying on code review.
package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath     = "github.com/Nomos-N4s/apivo-news"
	internalPrefix = modulePath + "/internal/"
	// platformModule is the shared bottom layer: any module may import it,
	// and it may import no sibling.
	platformModule = "platform"
)

// violation is one import that breaks a boundary rule: the file that holds
// the import, and the reason the import is refused.
type violation struct {
	file   string
	reason string
}

// TestModuleBoundaries walks every Go file under internal/ and asserts:
//
//  1. a module never imports another module's internals - modules
//     communicate through interfaces defined by the consumer and wired in
//     cmd; and
//  2. platform imports no sibling module - it is the bottom layer, holding
//     shared primitives only.
func TestModuleBoundaries(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	assertModulePath(t, root)

	found, err := checkInternal(os.DirFS(filepath.Join(root, "internal")))
	if err != nil {
		t.Fatalf("scanning internal/: %v", err)
	}
	for _, v := range found {
		t.Errorf("%s: %s", v.file, v.reason)
	}
}

// checkInternal parses every Go file in fsys - a filesystem rooted at
// internal/ - and reports each import that breaks a boundary rule.
//
// It is driven by the package graph it is handed rather than by a list of
// module names, so a module added tomorrow is governed by the same rules
// without this file being edited, and a module that does not exist yet
// cannot make a rule pass vacuously.
func checkInternal(fsys fs.FS) ([]violation, error) {
	fset := token.NewFileSet()
	var found []violation

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		owner := path.Dir(p)
		for _, imp := range file.Imports {
			target, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if reason, bad := violates(owner, target); bad {
				found = append(found, violation{file: p, reason: reason})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// violates answers whether ownerPkg - a package path relative to internal/ -
// may import importPath, and states why not when it may not. Imports of
// anything outside this module are never the architecture test's business.
func violates(ownerPkg, importPath string) (string, bool) {
	if !strings.HasPrefix(importPath, internalPrefix) {
		return "", false
	}

	ownerModule := moduleOf(ownerPkg)
	targetModule := moduleOf(strings.TrimPrefix(importPath, internalPrefix))
	if targetModule == ownerModule {
		return "", false
	}
	if ownerModule == platformModule {
		return fmt.Sprintf("platform must not import sibling module %q (import %q)", targetModule, importPath), true
	}
	if targetModule != platformModule {
		return fmt.Sprintf("module %q must not import module %q internals (import %q); communicate through consumer-defined interfaces wired in cmd", ownerModule, targetModule, importPath), true
	}
	return "", false
}

// assertModulePath guards against a module rename silently blinding this
// test: if go.mod declared a different module, internalPrefix would match no
// import and every rule would pass vacuously.
func assertModulePath(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "module ")
		if !ok {
			continue
		}
		if got := strings.TrimSpace(name); got != modulePath {
			t.Fatalf("go.mod declares module %q but this test enforces %q; update the modulePath constant", got, modulePath)
		}
		return
	}
	t.Fatal("no module line found in go.mod")
}

// moduleOf returns the first path segment: the owning module name.
func moduleOf(slashPath string) string {
	if i := strings.Index(slashPath, "/"); i >= 0 {
		return slashPath[:i]
	}
	return slashPath
}

// repoRoot ascends from the working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found in any parent directory")
		}
		dir = parent
	}
}
