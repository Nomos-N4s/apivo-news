// Package arch contains architecture conformance tests. The module
// boundaries of the modular monolith are enforced here so that a violating
// import fails the build in CI instead of relying on code review.
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
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
	internalDir := filepath.Join(root, "internal")
	fset := token.NewFileSet()

	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(internalDir, path)
		if err != nil {
			return err
		}
		owner := moduleOf(filepath.ToSlash(rel))

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			target, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(target, internalPrefix) {
				continue
			}
			targetModule := moduleOf(strings.TrimPrefix(target, internalPrefix))
			if targetModule == owner {
				continue
			}
			if owner == platformModule {
				t.Errorf("%s: platform must not import sibling module %q (import %q)", rel, targetModule, target)
				continue
			}
			if targetModule != platformModule {
				t.Errorf("%s: module %q must not import module %q internals (import %q); communicate through consumer-defined interfaces wired in cmd", rel, owner, targetModule, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
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
