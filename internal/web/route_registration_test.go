package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAdminRoutesAreRegisteredWithAdminOnly(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "routes.go", nil, 0)
	if err != nil {
		t.Fatalf("parse routes.go: %v", err)
	}
	foundAdminRoute := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		if !isMuxRouteRegistration(call.Fun) {
			return true
		}
		pattern, ok := stringArg(call.Args[0])
		if !ok || !strings.HasPrefix(pattern, "/admin") {
			return true
		}
		foundAdminRoute = true
		if !isAdminOnlyWrapper(call.Args[1]) {
			t.Errorf("%s registers admin route %q without adminOnly", fset.Position(call.Pos()), pattern)
		}
		return true
	})
	if !foundAdminRoute {
		t.Fatal("no admin routes found in routes.go")
	}
}

func TestStaticAssetsUseLongLivedCacheHeaders(t *testing.T) {
	server := NewServer(nil, nil, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestStaticSVGAssetsUseStandardContentType(t *testing.T) {
	server := NewServer(nil, nil, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/claude.svg", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Fatalf("Content-Type = %q, want %q", got, "image/svg+xml")
	}
}

func isMuxRouteRegistration(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	target, ok := selector.X.(*ast.Ident)
	if !ok || target.Name != "mux" {
		return false
	}
	return selector.Sel.Name == "Handle" || selector.Sel.Name == "HandleFunc"
}

func stringArg(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func isAdminOnlyWrapper(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "adminOnly"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "requireAdminOnly"
	default:
		return false
	}
}
