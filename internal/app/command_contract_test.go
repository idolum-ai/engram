package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/idolum-ai/engram/internal/commands"
)

func TestHandledCommandsHaveMetadata(t *testing.T) {
	t.Parallel()

	handled := handledCommandCases(t)
	for cmd := range handled {
		if _, ok := commands.Find(cmd); !ok {
			if !hiddenCompatibilityCommands[cmd] {
				t.Fatalf("handled command %q is neither public metadata nor an allowed hidden compatibility command", cmd)
			}
		}
	}
}

var hiddenCompatibilityCommands = map[string]bool{
	"run":               true,
	"type":              true,
	"stop":              true,
	"attachment-bypass": true,
}

func TestCommandMetadataIsHandled(t *testing.T) {
	t.Parallel()

	handled := handledCommandCases(t)
	for _, meta := range commands.All() {
		if !handled[meta.Command] {
			t.Fatalf("command metadata %q is not handled by app", meta.Command)
		}
	}
}

func handledCommandCases(t *testing.T) map[string]bool {
	t.Helper()

	path := filepath.Join(repoRootForAppTest(t), "internal", "app", "app.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "handleCommand" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			sw, ok := node.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			for _, stmt := range sw.Body.List {
				c, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range c.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[value] = true
				}
			}
			return false
		})
	}
	if len(out) == 0 {
		t.Fatal("no handleCommand switch cases found")
	}
	return out
}

func repoRootForAppTest(t *testing.T) string {
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
