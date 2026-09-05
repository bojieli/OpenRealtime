package httpclient_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// liveHTTPTrees are the packages that reach a model, a service, or a browser
// while someone is waiting. They are named rather than discovered because the
// claim is about them: the benchmark and review trees may build whatever
// client they like, and a test that swept the whole repository would fail on
// those for no reason and be relaxed until it meant nothing.
var liveHTTPTrees = []string{"adapters", "providers", "policymodel", "computeruse", "perception", "sidecar"}

// TestNoLiveAdapterBuildsItsOwnHTTPClient is a regression guard with a
// specific regression in mind.
//
// &http.Client{} is the obvious thing to write and it silently inherits
// http.DefaultTransport, whose MaxIdleConnsPerHost is 2. Every adapter in this
// repository had written it, and the effect - a redial and a TLS handshake for
// every concurrent call past the second, on the path a person is waiting on -
// is invisible in a test, invisible in a review, and invisible on a
// development machine talking to localhost.
//
// So it is checked in the source rather than in behaviour. There is no runtime
// signal that distinguishes an adapter with a warm pool from one without: both
// answer correctly, and only one of them is fast under load.
func TestNoLiveAdapterBuildsItsOwnHTTPClient(t *testing.T) {
	t.Parallel()
	var offenders []string
	for _, tree := range liveHTTPTrees {
		root := filepath.Join("..", "..", tree)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fileSet := token.NewFileSet()
			file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.CompositeLit:
					if isHTTPClientLiteral(typed.Type) {
						offenders = append(offenders, positionOf(fileSet, typed.Pos())+": &http.Client{...}")
					}
				case *ast.SelectorExpr:
					if identifierIs(typed.X, "http") && typed.Sel.Name == "DefaultClient" {
						offenders = append(offenders, positionOf(fileSet, typed.Pos())+": http.DefaultClient")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("these live paths build their own HTTP client and so inherit a two-connection "+
			"idle pool per host; use httpclient.Shared or httpclient.WithTimeout:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func isHTTPClientLiteral(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && identifierIs(selector.X, "http") && selector.Sel.Name == "Client"
}

func identifierIs(expr ast.Expr, name string) bool {
	identifier, ok := expr.(*ast.Ident)
	return ok && identifier.Name == name
}

func positionOf(fileSet *token.FileSet, pos token.Pos) string {
	position := fileSet.Position(pos)
	return filepath.ToSlash(strings.TrimPrefix(position.Filename, "../../")) +
		":" + strconv.Itoa(position.Line)
}
