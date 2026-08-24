package brand

// The committed TypeScript declaration is generated from the Go types in
// this package, and this file is what keeps the two from drifting apart.
// It is the same bargain the repository already makes for the sqlc store
// and the Supabase types: generated code is committed so it can be read
// and reviewed, and a job regenerates it and fails on a difference.

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// declarationPath is the committed declaration, relative to this package.
var declarationPath = filepath.Join("..", "..", "..", "web", "src", "lib", "brand", "brand.types.ts")

// updateDeclaration rewrites the committed file instead of asserting
// against it. It is how the file is regenerated after a change to the Go
// types: `go test ./internal/platform/brand/ -run TypeScriptDeclaration
// -update`.
var updateDeclaration = flag.Bool("update", false, "rewrite the generated TypeScript declaration")

func TestTypeScriptDeclarationIsCommittedAndCurrent(t *testing.T) {
	generated, err := TypeScriptTypes()
	if err != nil {
		t.Fatalf("TypeScriptTypes: %v", err)
	}

	if *updateDeclaration {
		if err := os.WriteFile(declarationPath, []byte(generated), 0o644); err != nil { //nolint:gosec // a committed, world-readable source file
			t.Fatalf("write %s: %v", declarationPath, err)
		}
		t.Logf("rewrote %s", declarationPath)
		return
	}

	committed, err := os.ReadFile(declarationPath)
	if err != nil {
		t.Fatalf("read %s: %v", declarationPath, err)
	}
	// .gitattributes normalises this file to LF in the repository and
	// Git hands it back with the platform's line endings, so the
	// comparison is about content rather than about which machine ran
	// it. Without this the check is red on every Windows checkout and
	// green in CI, which is the worst of both.
	if normaliseNewlines(string(committed)) != normaliseNewlines(generated) {
		t.Errorf("%s has drifted from the Go types it is generated from.\n"+
			"Regenerate it: go test ./internal/platform/brand/ -run TypeScriptDeclaration -update",
			declarationPath)
	}
}

// normaliseNewlines removes the carriage returns a Windows checkout adds.
func normaliseNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
