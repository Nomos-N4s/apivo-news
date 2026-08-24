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
	"testing/fstest"
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

func (v violation) String() string { return v.file + ": " + v.reason }

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

// boundaryCase is one fixture package graph - a single file at file,
// importing imports - and the verdict the rules must reach on it. An empty
// want means the imports are legal and must not be reported.
type boundaryCase struct {
	name    string
	file    string
	imports []string
	want    string
}

// TestModuleBoundaryRules proves the rules actually fire. TestModuleBoundaries
// above runs them against a repository that obeys them, so on its own it
// would stay green if a rule were deleted, mistyped or made unreachable. Each
// case below is a package graph that breaks exactly one rule - or deliberately
// does not break any - checked against the same code the real scan uses.
func TestModuleBoundaryRules(t *testing.T) {
	t.Parallel()

	tests := []boundaryCase{
		{
			name:    "a module importing another module's internals",
			file:    "editorial/queue.go",
			imports: []string{internalPrefix + "content/store"},
			want:    `module "editorial" must not import module "content"`,
		},
		{
			name:    "platform importing a sibling module",
			file:    "platform/http/router.go",
			imports: []string{internalPrefix + "editorial"},
			want:    `platform must not import sibling module "editorial"`,
		},
		{
			name:    "a module name that is a prefix of another module's",
			file:    "content/feed.go",
			imports: []string{internalPrefix + "contentious/store"},
			want:    `module "content" must not import module "contentious"`,
		},
		{
			name:    "any module importing platform",
			file:    "ingestion/poller.go",
			imports: []string{internalPrefix + "platform/db", internalPrefix + "platform/logging"},
		},
		{
			name:    "a package importing its own module",
			file:    "editorial/store/queries.go",
			imports: []string{internalPrefix + "editorial"},
		},
		{
			name:    "imports from outside this module",
			file:    "ingestion/feed.go",
			imports: []string{"strings", "github.com/mmcdole/gofeed"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			found, err := checkInternal(graphOf(tc.file, tc.imports))
			if err != nil {
				t.Fatalf("scanning the fixture graph: %v", err)
			}
			if tc.want == "" {
				if len(found) != 0 {
					t.Fatalf("imports the rules allow were refused: %v", found)
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("want exactly one violation mentioning %q, got %d: %v", tc.want, len(found), found)
			}
			if !strings.Contains(found[0].reason, tc.want) {
				t.Errorf("refused for the wrong reason:\n got: %s\nwant a reason containing: %s", found[0].reason, tc.want)
			}
			if found[0].file != tc.file {
				t.Errorf("violation blamed %q, want %q", found[0].file, tc.file)
			}
		})
	}
}

// graphOf builds a one-file package graph rooted where internal/ would be.
// parser.ImportsOnly needs no more than a package clause and an import
// block, which keeps the fixtures above readable as a table.
func graphOf(file string, imports []string) fstest.MapFS {
	var src strings.Builder
	fmt.Fprintf(&src, "package %s\n\nimport (\n", path.Base(path.Dir(file)))
	for _, imp := range imports {
		fmt.Fprintf(&src, "\t%q\n", imp)
	}
	src.WriteString(")\n")
	return fstest.MapFS{file: &fstest.MapFile{Data: []byte(src.String())}}
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
