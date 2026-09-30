package auth_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretName matches the last identifier of an expression that holds credential
// material.
var secretName = regexp.MustCompile(`(?i)(token|hash|csrf|secret|code|password|passwd|key|plain)$`)

// reviewed lists comparisons that match secretName but compare no attacker input.
var reviewed = map[string]string{
	"s.setupCode == expected":       "the server's own code against its snapshot, after the constant-time check",
	"p.Method == auth.MethodAPIKey": "authentication method, not a credential",
}

// TestSecretsAreComparedInConstantTime scans the packages that check credentials for
// == or != on credential material (tokens, hashes, CSRF tokens, setup codes, keys).
// Such comparisons must go through crypto/subtle, whose running time does not depend
// on where the values first differ; bcrypt already compares in constant time.
func TestSecretsAreComparedInConstantTime(t *testing.T) {
	dirs := []string{".", filepath.Join("..", "server"), filepath.Join("..", "secretbox"), filepath.Join("..", "mcp")}
	checked, seen := 0, map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			checked++
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				be, ok := n.(*ast.BinaryExpr)
				if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
					return true
				}
				if harmless(be.X) || harmless(be.Y) || (!secretName.MatchString(lastName(be.X)) && !secretName.MatchString(lastName(be.Y))) {
					return true
				}
				expr := render(fset, be)
				seen[expr] = true
				if _, ok := reviewed[expr]; !ok {
					t.Errorf("%s: %s compares credential material with %s; use crypto/subtle.ConstantTimeCompare",
						fset.Position(be.Pos()), expr, be.Op)
				}
				return true
			})
		}
	}
	if checked < 5 {
		t.Fatalf("checked only %d files", checked)
	}
	for expr := range reviewed {
		if !seen[expr] {
			t.Errorf("reviewed comparison %q no longer exists; remove it", expr)
		}
	}
}

// TestConstantTimeHelpersAreUsed pins the places that must use crypto/subtle.
func TestConstantTimeHelpersAreUsed(t *testing.T) {
	for file, want := range map[string]int{"credentials.go": 1, "service.go": 2} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(raw, []byte("subtle.ConstantTimeCompare(")); got < want {
			t.Errorf("%s calls subtle.ConstantTimeCompare %d times; want at least %d (API key hash, setup code, CSRF token)", file, got, want)
		}
	}
}

// harmless reports whether e is an operand a secret may be compared with by ==:
// literals (the empty string, lengths, sentinel runes), nil and len(...) calls,
// which reveal nothing about the value.
func harmless(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.CallExpr:
		fn, ok := v.Fun.(*ast.Ident)
		return ok && fn.Name == "len"
	}
	return false
}

// lastName returns the final identifier of an identifier or selector expression, ""
// for anything else (index expressions compare single characters of formats).
func lastName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.StarExpr:
		return lastName(v.X)
	}
	return ""
}

func render(fset *token.FileSet, n ast.Node) string {
	var buf bytes.Buffer
	_ = printer.Fprint(&buf, fset, n)
	return buf.String()
}
