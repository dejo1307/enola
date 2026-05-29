package goextractor

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// routeInfo holds a detected route registration.
type routeInfo struct {
	method    string // "GET", "POST", etc. or "ALL"
	path      string // e.g. "/api/registration-enabled"
	handler   string // e.g. "app.Handlers.User.CreatePasswordReset"
	framework string // "gorilla/mux", "chi", "net/http"
	line      int
}

// extractRoutes walks function bodies in a Go file looking for HTTP route registrations.
func extractRoutes(fset *token.FileSet, f *ast.File, relFile, pkgDir string) []facts.Fact {
	var result []facts.Fact

	// Detect which router framework is imported
	framework := detectRouterFramework(f)
	if framework == "" {
		return nil
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		// Track subrouter prefix assignments: varName -> prefix
		prefixes := make(map[string]string)

		for _, stmt := range fn.Body.List {
			extractRoutesFromStmt(fset, stmt, prefixes, framework, relFile, pkgDir, &result)
		}
	}

	return result
}

// detectRouterFramework checks imports to determine which router framework is used.
// Specific frameworks (gorilla/mux, chi) take priority over net/http.
func detectRouterFramework(f *ast.File) string {
	hasNetHTTP := false
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		switch {
		case strings.Contains(path, "gorilla/mux"):
			return "gorilla/mux"
		case strings.Contains(path, "go-chi/chi"):
			return "chi"
		case path == "net/http":
			hasNetHTTP = true
		}
	}
	if hasNetHTTP {
		return "net/http"
	}
	return ""
}

// extractRoutesFromStmt processes a single statement looking for route registrations and subrouter assignments.
func extractRoutesFromStmt(fset *token.FileSet, stmt ast.Stmt, prefixes map[string]string, framework, relFile, pkgDir string, result *[]facts.Fact) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		if call, ok := s.X.(*ast.CallExpr); ok {
			routes := extractRoutesFromCall(fset, call, prefixes, framework)
			for _, r := range routes {
				*result = append(*result, routeToFact(r, relFile, pkgDir))
			}
			// Recurse into function literal arguments (e.g. r.Group(func(r chi.Router) { r.Get(...) })).
			// This captures routes registered inside oapi-codegen's HandlerWithOptions pattern.
			for _, arg := range call.Args {
				if fn, ok := arg.(*ast.FuncLit); ok && fn.Body != nil {
					for _, inner := range fn.Body.List {
						extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
					}
				}
			}
		}

	case *ast.AssignStmt:
		// Track subrouter assignments: apiRouter := router.PathPrefix("/api").Subrouter()
		if len(s.Lhs) == 1 && len(s.Rhs) == 1 {
			if ident, ok := s.Lhs[0].(*ast.Ident); ok {
				if prefix := extractSubrouterPrefix(s.Rhs[0]); prefix != "" {
					// Resolve parent prefix
					parentVar := extractReceiverVar(s.Rhs[0])
					if parentPrefix, ok := prefixes[parentVar]; ok {
						prefix = parentPrefix + prefix
					}
					prefixes[ident.Name] = prefix
				}
			}
		}
		// Also check for route registrations in assignments (e.g. _ = router.HandleFunc(...))
		for _, rhs := range s.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				routes := extractRoutesFromCall(fset, call, prefixes, framework)
				for _, r := range routes {
					*result = append(*result, routeToFact(r, relFile, pkgDir))
				}
			}
		}

	case *ast.IfStmt:
		// Walk into if bodies for conditional route registration
		if s.Body != nil {
			for _, inner := range s.Body.List {
				extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
			}
		}
		if s.Else != nil {
			if block, ok := s.Else.(*ast.BlockStmt); ok {
				for _, inner := range block.List {
					extractRoutesFromStmt(fset, inner, prefixes, framework, relFile, pkgDir, result)
				}
			}
		}
	}
}

// extractRoutesFromCall extracts route info from a call expression.
func extractRoutesFromCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string, framework string) []routeInfo {
	switch framework {
	case "gorilla/mux":
		return extractGorillaMuxRoutes(fset, call, prefixes)
	case "chi":
		return extractChiRoutes(fset, call, prefixes)
	case "net/http":
		return extractNetHTTPRoutes(fset, call)
	}
	return nil
}

// extractGorillaMuxRoutes handles Gorilla Mux patterns.
func extractGorillaMuxRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	// Pattern 1: router.HandleFunc("/path", handler).Methods("GET")
	// AST: CallExpr{Fun: SelectorExpr{X: CallExpr{HandleFunc}, Sel: Methods}, Args: ["GET"]}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Methods" {
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			method := extractStringArg(call, 0)
			ri := extractHandleFuncCall(fset, innerCall, prefixes)
			if ri != nil {
				ri.method = strings.ToUpper(method)
				ri.framework = "gorilla/mux"
				return []routeInfo{*ri}
			}
		}
		return nil
	}

	// Pattern 2: router.HandleFunc("/path", handler) without .Methods()
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		switch sel.Sel.Name {
		case "HandleFunc", "Handle":
			ri := extractHandleFuncCall(fset, call, prefixes)
			if ri != nil {
				ri.method = "ALL"
				ri.framework = "gorilla/mux"
				return []routeInfo{*ri}
			}

		case "Use":
			// router.Use(middleware)
			if len(call.Args) > 0 {
				handler := exprToString(call.Args[0])
				if handler != "" {
					receiverVar := identName(sel.X)
					prefix := prefixes[receiverVar]
					return []routeInfo{{
						method:    "USE",
						path:      prefix,
						handler:   handler,
						framework: "gorilla/mux",
						line:      fset.Position(call.Pos()).Line,
					}}
				}
			}
		}
	}

	return nil
}

// extractChiRoutes handles Chi router patterns like r.Get("/path", handler).
func extractChiRoutes(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	methodName := sel.Sel.Name
	var httpMethod string

	switch methodName {
	case "Get":
		httpMethod = "GET"
	case "Post":
		httpMethod = "POST"
	case "Put":
		httpMethod = "PUT"
	case "Delete":
		httpMethod = "DELETE"
	case "Patch":
		httpMethod = "PATCH"
	case "Head":
		httpMethod = "HEAD"
	case "Options":
		httpMethod = "OPTIONS"
	case "HandleFunc", "Handle":
		httpMethod = "ALL"
	case "Use":
		if len(call.Args) > 0 {
			handler := exprToString(call.Args[0])
			if handler != "" {
				receiverVar := identName(sel.X)
				prefix := prefixes[receiverVar]
				return []routeInfo{{
					method:    "USE",
					path:      prefix,
					handler:   handler,
					framework: "chi",
					line:      fset.Position(call.Pos()).Line,
				}}
			}
		}
		return nil
	default:
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	if path == "" {
		return nil
	}

	receiverVar := identName(sel.X)
	if prefix, ok := prefixes[receiverVar]; ok {
		path = prefix + path
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return []routeInfo{{
		method:    httpMethod,
		path:      path,
		handler:   handler,
		framework: "chi",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// extractNetHTTPRoutes handles net/http patterns like http.HandleFunc("/path", handler).
func extractNetHTTPRoutes(fset *token.FileSet, call *ast.CallExpr) []routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
		return nil
	}

	// Check it's called on http or a mux variable
	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	if path == "" {
		return nil
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return []routeInfo{{
		method:    "ALL",
		path:      path,
		handler:   handler,
		framework: "net/http",
		line:      fset.Position(call.Pos()).Line,
	}}
}

// extractHandleFuncCall extracts path and handler from a HandleFunc/Handle call.
func extractHandleFuncCall(fset *token.FileSet, call *ast.CallExpr, prefixes map[string]string) *routeInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
		return nil
	}

	if len(call.Args) < 1 {
		return nil
	}

	path := extractStringArg(call, 0)
	if path == "" {
		return nil
	}

	// Resolve prefix from receiver variable
	receiverVar := identName(sel.X)
	if prefix, ok := prefixes[receiverVar]; ok {
		path = prefix + path
	}

	handler := ""
	if len(call.Args) >= 2 {
		handler = exprToString(call.Args[1])
	}

	return &routeInfo{
		path:    path,
		handler: handler,
		line:    fset.Position(call.Pos()).Line,
	}
}

// extractSubrouterPrefix extracts the path prefix from a PathPrefix(...).Subrouter() chain.
func extractSubrouterPrefix(expr ast.Expr) string {
	// Pattern: router.PathPrefix("/api").Subrouter()
	// AST: CallExpr{Fun: SelectorExpr{X: CallExpr{PathPrefix}, Sel: Subrouter}}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}

	if sel.Sel.Name == "Subrouter" {
		// The receiver should be PathPrefix(...)
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			if innerSel, ok := innerCall.Fun.(*ast.SelectorExpr); ok {
				if innerSel.Sel.Name == "PathPrefix" && len(innerCall.Args) >= 1 {
					return extractStringArg(innerCall, 0)
				}
			}
		}
	}

	return ""
}

// extractReceiverVar returns the variable name that the Subrouter() chain is called on.
func extractReceiverVar(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if sel.Sel.Name == "Subrouter" {
		if innerCall, ok := sel.X.(*ast.CallExpr); ok {
			if innerSel, ok := innerCall.Fun.(*ast.SelectorExpr); ok {
				return identName(innerSel.X)
			}
		}
	}
	return ""
}

// extractStringArg returns the string value of the argument at the given index, or "".
func extractStringArg(call *ast.CallExpr, index int) string {
	if index >= len(call.Args) {
		return ""
	}
	return extractStringExpr(call.Args[index])
}

// extractStringExpr extracts a string value from an expression.
// Handles plain string literals and binary concatenation (e.g. options.BaseURL+"/path").
// For concatenations where only the right side is a path literal (starts with "/"),
// the path portion is returned so oapi-codegen patterns resolve correctly.
func extractStringExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.Trim(e.Value, `"`)
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			right := extractStringExpr(e.Y)
			// When the right side is an absolute path ("/..."), use it directly.
			// This correctly handles `options.BaseURL + "/api/v1/..."` where BaseURL
			// is a runtime variable that is typically empty or a host prefix.
			if strings.HasPrefix(right, "/") {
				return right
			}
			left := extractStringExpr(e.X)
			return left + right
		}
	}
	return ""
}

// exprToString converts an expression to a human-readable string.
// Handles selector chains like app.Handlers.User.CreatePasswordReset.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		x := exprToString(e.X)
		if x != "" {
			return x + "." + e.Sel.Name
		}
		return e.Sel.Name
	case *ast.CallExpr:
		// For function call expressions like middleware.RateLimitMiddleware(...)
		return exprToString(e.Fun)
	}
	return ""
}

// identName returns the name of an identifier expression, or "".
func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// routeToFact converts a routeInfo to a facts.Fact.
func routeToFact(r routeInfo, relFile, pkgDir string) facts.Fact {
	props := map[string]any{
		"method":    r.method,
		"framework": r.framework,
		"language":  "go",
	}
	if r.handler != "" {
		props["handler"] = r.handler
	}
	if r.method == "USE" {
		props["type"] = "middleware"
	}

	return facts.Fact{
		Kind: facts.KindRoute,
		Name: r.path,
		File: relFile,
		Line: r.line,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}
}
