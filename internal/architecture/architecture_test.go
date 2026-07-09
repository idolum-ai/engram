package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageImportBoundaries(t *testing.T) {
	t.Parallel()

	rules := []struct {
		dir       string
		forbidden []string
	}{
		{
			dir: "internal/telegram",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/tmux",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
			},
		},
		{
			dir: "internal/anthropic",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/commands",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
	}

	root := repoRoot(t)
	for _, rule := range rules {
		rule := rule
		t.Run(rule.dir, func(t *testing.T) {
			t.Parallel()
			forbidden := map[string]bool{}
			for _, imp := range rule.forbidden {
				forbidden[imp] = true
			}
			assertNoForbiddenImports(t, filepath.Join(root, rule.dir), forbidden)
		})
	}
}

func assertNoForbiddenImports(t *testing.T, dir string, forbidden map[string]bool) {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse imports %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			imp := strings.Trim(spec.Path.Value, "\"")
			if forbidden[imp] {
				t.Fatalf("%s imports forbidden package %s", path, imp)
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("could not find repo root")
		}
		wd = parent
	}
}
