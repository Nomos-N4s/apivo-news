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
	// cmdPrefix is the composition root. Wiring flows one way: cmd knows
	// every domain, and no domain knows cmd.
	cmdPrefix = modulePath + "/cmd/"
	// platformModule is the shared bottom layer: any module may import it,
	// and it may import no sibling.
	platformModule = "platform"
	// identityModule is the shared account layer above platform: any
	// product domain may import it, and it imports only platform
	// (ADR-0001). Accounts are the one thing both products genuinely share,
	// so the alternative to this exception is every domain carrying its own
	// notion of who a member is.
	identityModule = "identity"
	// networksDir is the directory a domain keeps its external network
	// adapters in - internal/<domain>/networks/<name>/ (ADR-0003). Every
	// directory below it is one adapter and a sealed unit: naming the
	// convention rather than the adapters means a network added tomorrow is
	// sealed the moment its directory exists, with nothing here to update.
	networksDir = "networks"
)

// violation is one import that breaks a boundary rule: the file that holds
// the import, and the reason the import is refused.
type violation struct {
	file   string
	reason string
}

func (v violation) String() string { return v.file + ": " + v.reason }

// scan is the result of one pass over a package graph. The counters are not
// decoration: a scan that parsed nothing, or that saw no import of this
// module at all, reports zero violations for the same reason a scan of a
// perfectly obedient repository does, and the two must not look alike.
type scan struct {
	violations []violation
	files      int
	imports    int
}

// TestModuleBoundaries walks every Go file under internal/ and asserts the
// import rules of ADR-0001:
//
//  1. platform may be imported by anyone and imports no domain - it is the
//     bottom layer, holding shared primitives only;
//  2. identity may be imported by any product domain and imports only
//     platform;
//  3. a product domain may not import another product domain, at any depth -
//     cross-product collaboration is asynchronous, through the domain event
//     stream, never a direct call; and
//  4. sub-packages of one product domain may import each other freely,
//     except that a network adapter under <domain>/networks/<name>/ is
//     sealed: nothing under internal/ imports it, so no network-specific
//     type escapes into the domain (SC-008); and
//  5. composition happens only in cmd.
func TestModuleBoundaries(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	assertModulePath(t, root)

	internalFS := os.DirFS(filepath.Join(root, "internal"))
	if err := checkLayers(internalFS); err != nil {
		t.Fatalf("the rules can no longer see what they govern: %v", err)
	}

	got, err := checkInternal(internalFS)
	if err != nil {
		t.Fatalf("scanning internal/: %v", err)
	}
	if got.files == 0 || got.imports == 0 {
		t.Fatalf("scanned %d Go file(s) holding %d import(s) of this module; the scan found nothing to judge, so every rule below passed vacuously", got.files, got.imports)
	}
	for _, v := range got.violations {
		t.Errorf("%s", v)
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
			name:    "a domain importing another domain's internals",
			file:    "editorial/queue.go",
			imports: []string{internalPrefix + "content/store"},
			want:    `domain "editorial" must not import domain "content"`,
		},
		{
			name:    "a product domain importing another product domain",
			file:    "cashback/earnings/attribute.go",
			imports: []string{internalPrefix + "content"},
			want:    `domain "cashback" must not import domain "content"`,
		},
		{
			name:    "a product domain reaching into another product domain from depth",
			file:    "cashback/networks/awin/client.go",
			imports: []string{internalPrefix + "editorial/store"},
			want:    `domain "cashback" must not import domain "editorial"`,
		},
		{
			name:    "a news domain reaching into a cashback sub-package",
			file:    "content/store/queries.go",
			imports: []string{internalPrefix + "cashback/wallet"},
			want:    `domain "content" must not import domain "cashback"`,
		},
		{
			name: "sub-packages of one product domain importing each other",
			file: "cashback/earnings/ledger.go",
			imports: []string{
				internalPrefix + "cashback/wallet",
				internalPrefix + "cashback/catalogue",
				internalPrefix + "cashback/networks",
			},
		},
		{
			name:    "platform importing a sibling module",
			file:    "platform/http/router.go",
			imports: []string{internalPrefix + "editorial"},
			want:    `platform must not import sibling module "editorial"`,
		},
		{
			name:    "a domain name that is a prefix of another domain's",
			file:    "content/feed.go",
			imports: []string{internalPrefix + "contentious/store"},
			want:    `domain "content" must not import domain "contentious"`,
		},
		{
			name:    "a domain package importing a network adapter",
			file:    "cashback/earnings/attribute.go",
			imports: []string{internalPrefix + "cashback/networks/awin"},
			want:    `must not import network adapter "cashback/networks/awin"`,
		},
		{
			name:    "one network adapter importing another",
			file:    "cashback/networks/awin/client.go",
			imports: []string{internalPrefix + "cashback/networks/tradedoubler"},
			want:    `must not import network adapter "cashback/networks/tradedoubler"`,
		},
		{
			name:    "the port importing an adapter that satisfies it",
			file:    "cashback/networks/port.go",
			imports: []string{internalPrefix + "cashback/networks/awin"},
			want:    `must not import network adapter "cashback/networks/awin"`,
		},
		{
			name:    "an adapter importing the port it satisfies",
			file:    "cashback/networks/awin/client.go",
			imports: []string{internalPrefix + "cashback/networks"},
		},
		{
			name:    "an adapter's own sub-package importing the adapter",
			file:    "cashback/networks/awin/fixtures/recorded.go",
			imports: []string{internalPrefix + "cashback/networks/awin"},
		},
		{
			name:    "a domain importing the composition root",
			file:    "cashback/ops/queues.go",
			imports: []string{cmdPrefix + "apivo"},
			want:    "must not import the composition root",
		},
		{
			name:    "identity importing a product domain",
			file:    "identity/service.go",
			imports: []string{internalPrefix + "content/store"},
			want:    `identity must not import module "content"`,
		},
		{
			name:    "identity importing platform",
			file:    "identity/verifier.go",
			imports: []string{internalPrefix + "platform/config"},
		},
		{
			name:    "a product domain importing identity",
			file:    "cashback/payout/approve.go",
			imports: []string{internalPrefix + "identity"},
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

			got, err := checkInternal(graphOf(tc.file, tc.imports))
			if err != nil {
				t.Fatalf("scanning the fixture graph: %v", err)
			}
			found := got.violations
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

// TestBoundaryScanRefusesToPassVacuously proves the anti-vacuity guard is
// itself worth having: each tree below would produce a clean scan while
// checking nothing, and must be refused as a broken test rather than
// reported as an obedient repository.
func TestBoundaryScanRefusesToPassVacuously(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dirs []string
		want string
	}{
		{
			name: "the layers and two domains",
			dirs: []string{"platform", "identity", "content", "cashback"},
		},
		{
			name: "platform renamed or removed",
			dirs: []string{"identity", "content", "cashback"},
			want: "internal/platform/ does not exist",
		},
		{
			name: "identity renamed or removed",
			dirs: []string{"platform", "content", "cashback"},
			want: "internal/identity/ does not exist",
		},
		{
			name: "nothing but the shared layers",
			dirs: []string{"platform", "identity"},
			want: "needs two domains",
		},
		{
			name: "a single domain, so nothing to cross",
			dirs: []string{"platform", "identity", "content"},
			want: "needs two domains",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tree := fstest.MapFS{}
			for _, dir := range tc.dirs {
				tree[dir+"/doc.go"] = &fstest.MapFile{Data: []byte("package " + path.Base(dir) + "\n")}
			}

			err := checkLayers(tree)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("a tree the rules can govern was refused: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("a tree that would pass vacuously was accepted; want a refusal mentioning %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("refused for the wrong reason:\n got: %v\nwant a reason containing: %s", err, tc.want)
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
func checkInternal(fsys fs.FS) (scan, error) {
	fset := token.NewFileSet()
	var got scan

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
		got.files++
		owner := path.Dir(p)
		for _, imp := range file.Imports {
			target, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(target, modulePath+"/") {
				got.imports++
			}
			if reason, bad := violates(owner, target); bad {
				got.violations = append(got.violations, violation{file: p, reason: reason})
			}
		}
		return nil
	})
	if err != nil {
		return scan{}, err
	}
	return got, nil
}

// checkLayers guards the part of the rule set that is named rather than
// derived. platform and identity are the only two module names this file
// spells out; if either directory is renamed or removed, every rule
// mentioning it stops matching and starts passing - silently, and most
// loudly on the day the rename lands, when nobody is looking for it. The
// domain count guards the same way from the other side: the cross-domain
// rule has nothing to forbid until two domains exist.
func checkLayers(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return err
	}

	present := make(map[string]bool, len(entries))
	domains := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		present[e.Name()] = true
		if e.Name() != platformModule && e.Name() != identityModule {
			domains = append(domains, e.Name())
		}
	}

	for _, layer := range []string{platformModule, identityModule} {
		if !present[layer] {
			return fmt.Errorf("internal/%s/ does not exist, so every rule naming it now matches nothing and passes without checking anything; point the constant at the directory that replaced it, or delete the rule it belongs to", layer)
		}
	}
	if len(domains) < 2 {
		return fmt.Errorf("found %d module(s) beside platform and identity (%v); the cross-domain rule needs two domains before it forbids anything", len(domains), domains)
	}
	return nil
}

// violates answers whether ownerPkg - a package path relative to internal/ -
// may import importPath, and states why not when it may not. Imports of
// anything outside this module are never the architecture test's business.
func violates(ownerPkg, importPath string) (string, bool) {
	if strings.HasPrefix(importPath, cmdPrefix) {
		return fmt.Sprintf("%s must not import the composition root (import %q); cmd wires the domains together and nothing under internal reaches back into the wiring", ownerPkg, importPath), true
	}
	if !strings.HasPrefix(importPath, internalPrefix) {
		return "", false
	}

	targetPkg := strings.TrimPrefix(importPath, internalPrefix)
	if adapter := adapterRoot(targetPkg); adapter != "" && !within(ownerPkg, adapter) {
		return fmt.Sprintf("%s must not import network adapter %q (import %q); a network's vocabulary never leaves its own package - take the port defined in %q and let cmd choose which adapter satisfies it, so adding a second network changes only its own adapter (SC-008)", ownerPkg, adapter, importPath, path.Dir(adapter)), true
	}

	ownerModule := moduleOf(ownerPkg)
	targetModule := moduleOf(targetPkg)
	switch {
	case targetModule == ownerModule:
		return "", false
	case ownerModule == platformModule:
		return fmt.Sprintf("platform must not import sibling module %q (import %q); platform is the bottom layer and imports no domain", targetModule, importPath), true
	case targetModule == platformModule:
		return "", false
	case ownerModule == identityModule:
		return fmt.Sprintf("identity must not import module %q (import %q); identity sits directly above platform and imports nothing else", targetModule, importPath), true
	case targetModule == identityModule:
		return "", false
	default:
		return fmt.Sprintf("domain %q must not import domain %q at any depth (import %q); product domains share platform and identity and nothing else, and integrate asynchronously through the domain event stream - define the interface you need inside your own domain and wire it in cmd", ownerModule, targetModule, importPath), true
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

// adapterRoot returns the network adapter package that slashPath belongs to
// - "<domain>/networks/<name>" - or "" when the path sits outside every
// adapter. The parent "<domain>/networks" holds the port, not an adapter, so
// it is deliberately not a root of itself.
func adapterRoot(slashPath string) string {
	segments := strings.Split(slashPath, "/")
	// From 1: an adapter always hangs off a domain, so a top-level
	// internal/networks/ would not be one.
	for i := 1; i+1 < len(segments); i++ {
		if segments[i] == networksDir {
			return strings.Join(segments[:i+2], "/")
		}
	}
	return ""
}

// within reports whether slashPath is root or sits underneath it.
func within(slashPath, root string) bool {
	return slashPath == root || strings.HasPrefix(slashPath, root+"/")
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
