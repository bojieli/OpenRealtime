package openrealtime_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// A debug category a client cannot select is an emitter that does not exist.
//
// The gateway forwards a trace entry only when its category is one the session
// negotiated, and a session can only negotiate categories this package
// advertises. An entry built with any other name is therefore dropped on the
// way out, silently, with no error and nothing in the log - the emitter looks
// implemented, the client sees nothing, and neither end can tell that apart
// from a subsystem that had nothing to say.
//
// Three graph-native bindings and one word-timing reporter were in exactly
// that state: everything they traced was discarded before it reached the wire.
// This reads the emitters rather than trusting them.
func TestEveryEmittedDebugCategoryIsOneAClientCanSelect(t *testing.T) {
	root := moduleRoot(t)
	known := openrealtime.DebugCategories()
	offenders := map[string][]string{}
	files := 0
	found := 0

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".runtime", "node_modules", "testdata", "vendor", "artifacts", "third_party":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return nil
		}
		files++
		relative, _ := filepath.Rel(root, path)
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.CompositeLit)
			if !isLiteral {
				return true
			}
			selector, isSelector := literal.Type.(*ast.SelectorExpr)
			if !isSelector || selector.Sel.Name != "DebugEvent" {
				return true
			}
			for _, element := range literal.Elts {
				pair, isPair := element.(*ast.KeyValueExpr)
				if !isPair {
					continue
				}
				key, isName := pair.Key.(*ast.Ident)
				if !isName || key.Name != "Category" {
					continue
				}
				// A typed constant cannot name a category that does not
				// exist. Only a bare string can, so only a bare string is
				// checked here.
				text, isText := pair.Value.(*ast.BasicLit)
				if !isText || text.Kind != token.STRING {
					continue
				}
				value, unquoteErr := strconv.Unquote(text.Value)
				if unquoteErr != nil {
					continue
				}
				found++
				if !slices.Contains(known, openrealtime.DebugCategory(value)) {
					position := fileSet.Position(text.Pos())
					offenders[value] = append(offenders[value],
						relative+":"+strconv.Itoa(position.Line))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files, which is too few to have read the tree", files)
	}
	if found == 0 {
		t.Fatal("found no debug category literal at all, so this test checked nothing")
	}
	if len(offenders) != 0 {
		for category, sites := range offenders {
			t.Errorf("debug category %q is emitted at %s but no client can select it; "+
				"every entry is dropped by the gateway's category filter",
				category, strings.Join(sites, ", "))
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test directory")
		}
		directory = parent
	}
}
