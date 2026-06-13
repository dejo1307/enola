package tsextractor

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// TSExtractor extracts architectural facts from TypeScript/TSX source code using tree-sitter.
type TSExtractor struct{}

// New creates a new TSExtractor.
func New() *TSExtractor {
	return &TSExtractor{}
}

func (e *TSExtractor) Name() string {
	return "typescript"
}

// Detect returns true if the repository (or one of its immediate subdirectories
// in the case of a monorepo) contains TypeScript markers.
func (e *TSExtractor) Detect(repoPath string) (bool, error) {
	_, found := findTSRoot(repoPath)
	return found, nil
}

// findTSRoot returns the directory that is the TypeScript project root, along
// with a boolean indicating whether one was found. It checks repoPath itself
// first, then one level of subdirectories to handle monorepos where the
// TypeScript code lives in a subfolder (e.g. a "client/" directory).
func findTSRoot(repoPath string) (string, bool) {
	if hasTSMarkers(repoPath) {
		return repoPath, true
	}

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return repoPath, false
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		sub := filepath.Join(repoPath, entry.Name())
		if hasTSMarkers(sub) {
			return sub, true
		}
	}
	return repoPath, false
}

// hasTSMarkers returns true if the directory looks like a TypeScript project root.
func hasTSMarkers(dir string) bool {
	// tsconfig.json (standard) or tsconfig.base.json (Nx monorepo)
	for _, name := range []string{"tsconfig.json", "tsconfig.base.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}

	// package.json with a typescript dependency
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := pkg[key].(map[string]any); ok {
			if _, ok := deps["typescript"]; ok {
				return true
			}
		}
	}
	return false
}

// Extract parses TypeScript/TSX files and emits architectural facts.
func (e *TSExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	// Detect if this is a Next.js project
	isNextJS := detectNextJS(repoPath)

	// Parse tsconfig.json for path alias mappings (e.g., "@/*" → "src/*")
	aliases := parseTSPathAliases(repoPath)

	// Group files by directory for module detection
	modules := make(map[string]bool)

	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isTypeScriptFile(relFile) {
			continue
		}

		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[ts-extractor] error reading %s: %v", relFile, err)
			continue
		}

		fileFacts := e.extractFile(src, relFile, isNextJS, aliases)
		allFacts = append(allFacts, fileFacts...)

		dir := filepath.Dir(relFile)
		modules[dir] = true
	}

	// Emit module facts for each directory
	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "typescript",
			},
		})
	}

	return allFacts, nil
}

// extractCtx bundles the per-file state threaded through declaration extraction
// so symbols can be enriched with React/Next.js semantic classification.
type extractCtx struct {
	src       []byte
	relFile   string
	dir       string
	isTSX     bool
	isNextJS  bool
	importMap map[string]string
}

func (e *TSExtractor) extractFile(src []byte, relFile string, isNextJS bool, aliases map[string]string) []facts.Fact {
	var result []facts.Fact

	// Parse openapi-typescript generated files for backend API route dependencies.
	// These are client-role route facts showing which backend routes the TS code calls.
	if openapiRoutes := extractOpenAPITypescriptFacts(src, relFile); len(openapiRoutes) > 0 {
		result = append(result, openapiRoutes...)
	}

	// Hand-written fetch / makeRequest API calls are also client-role routes.
	result = append(result, extractHTTPClientFacts(src, relFile)...)

	isTSX := strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx")
	lang := typescript.LanguageTypescript()
	if isTSX {
		lang = typescript.LanguageTSX()
	}

	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(sitter.NewLanguage(lang))

	tree := parser.Parse(src, nil)
	defer tree.Close()

	root := tree.RootNode()

	// Extract from the tree
	result = append(result, e.extractImports(root, src, relFile, aliases)...)

	ctx := &extractCtx{
		src:       src,
		relFile:   relFile,
		dir:       filepath.Dir(relFile),
		isTSX:     isTSX,
		isNextJS:  isNextJS,
		importMap: buildImportSymbols(root, src, relFile, aliases),
	}
	decls := e.extractDeclarations(root, ctx)

	// A declaration may be exported via a separate `export { A, B }` clause or
	// `export default Name` statement rather than an inline `export` keyword.
	// Mark the corresponding symbols as exported.
	if exported := collectExportedLocalNames(root, src); len(exported) > 0 {
		for i := range decls {
			if decls[i].Kind != facts.KindSymbol {
				continue
			}
			local := decls[i].Name[strings.LastIndexByte(decls[i].Name, '.')+1:]
			if exported[local] {
				decls[i].Props["exported"] = true
			}
		}
	}
	result = append(result, decls...)

	// Detect Next.js routes
	if isNextJS {
		if routeFact := detectRoute(relFile); routeFact != nil {
			result = append(result, *routeFact)
		}
	}

	return result
}

func (e *TSExtractor) extractImports(root *sitter.Node, src []byte, relFile string, aliases map[string]string) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)

	for i := range root.ChildCount() {
		child := root.Child(i)
		if child.Kind() != "import_statement" {
			continue
		}

		// Find the import source (string)
		source := findChildByKind(child, "string")
		if source == nil {
			continue
		}

		importPath := strings.Trim(nodeText(source, src), `"'`)

		// Resolve path aliases and relative imports to filesystem-relative paths
		resolved, isExternal := resolveImportPath(importPath, dir, aliases)

		importSource := "internal"
		if isExternal {
			importSource = "external"
		}

		result = append(result, facts.Fact{
			Kind: facts.KindDependency,
			Name: dir + " -> " + resolved,
			File: relFile,
			Line: int(child.StartPosition().Row) + 1,
			Props: map[string]any{
				"language": "typescript",
				"source":   importSource,
			},
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: resolved},
			},
		})
	}

	return result
}

func (e *TSExtractor) extractDeclarations(root *sitter.Node, ctx *extractCtx) []facts.Fact {
	var result []facts.Fact
	for i := range root.ChildCount() {
		result = append(result, e.extractNode(root.Child(i), ctx, false, "")...)
	}
	return result
}

// extractNode emits facts for a single declaration node. fallbackName supplies a
// name for anonymous default-exported declarations (e.g. `export default function
// () {}`), derived from the file name; it is ignored when the declaration has its
// own name.
func (e *TSExtractor) extractNode(node *sitter.Node, ctx *extractCtx, isExported bool, fallbackName string) []facts.Fact {
	var result []facts.Fact
	src, dir, relFile := ctx.src, ctx.dir, ctx.relFile

	switch node.Kind() {
	case "export_statement":
		isDefault := hasChildKind(node, "default")
		fb := ""
		if isDefault {
			fb = fileSymbolName(relFile)
		}
		// Named/inline declaration inside the export.
		if decl := firstDeclChild(node); decl != nil {
			return e.extractNode(decl, ctx, true, fb)
		}
		// Anonymous default export of a value: name it after the file.
		if isDefault {
			for _, k := range []string{"function_expression", "generator_function", "class", "arrow_function", "call_expression"} {
				if c := findChildByKind(node, k); c != nil {
					return e.extractNode(c, ctx, true, fb)
				}
			}
		}

	case "function_declaration", "function_expression", "generator_function_declaration", "generator_function":
		name := findChildByKind(node, "identifier")
		symbolName := fallbackName
		if name != nil {
			symbolName = nodeText(name, src)
		}
		if symbolName == "" {
			break
		}
		result = append(result, e.funcSymbol(node, node, ctx, symbolName, isExported))

	case "arrow_function":
		if fallbackName != "" {
			result = append(result, e.funcSymbol(node, node, ctx, fallbackName, isExported))
		}

	case "call_expression":
		// Reached for `export default memo(...)` / `forwardRef(...)`.
		if fallbackName != "" {
			result = append(result, e.funcSymbol(node, node, ctx, fallbackName, isExported))
		}

	case "class_declaration", "abstract_class_declaration", "class":
		name := findChildByKind(node, "type_identifier")
		symbolName := fallbackName
		if name != nil {
			symbolName = nodeText(name, src)
		}
		if symbolName == "" {
			break
		}
		f := facts.Fact{
			Kind: facts.KindSymbol,
			Name: dir + "." + symbolName,
			File: relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": facts.SymbolClass,
				"exported":    isExported,
				"language":    "typescript",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: dir},
			},
		}

		// Check for implements clause (nested under class_heritage)
		for j := range node.ChildCount() {
			c := node.Child(j)
			if c.Kind() == "class_heritage" {
				for k := range c.ChildCount() {
					heritage := c.Child(k)
					if heritage.Kind() == "implements_clause" {
						for l := range heritage.ChildCount() {
							t := heritage.Child(l)
							if t.Kind() == "type_identifier" {
								f.Relations = append(f.Relations, facts.Relation{
									Kind:   facts.RelImplements,
									Target: nodeText(t, src),
								})
							}
						}
					}
				}
			}
		}

		classBody := findChildByKind(node, "class_body")
		classifySymbol(&f, symbolName, classBody, ctx, facts.SymbolClass)
		result = append(result, f)

		// Extract class methods
		if classBody != nil {
			for j := range classBody.ChildCount() {
				member := classBody.Child(j)
				if member.Kind() != "method_definition" && member.Kind() != "public_field_definition" {
					continue
				}
				methodName := findChildByKind(member, "property_identifier")
				if methodName == nil {
					methodName = findChildByKind(member, "identifier")
				}
				if methodName == nil {
					continue
				}
				mName := nodeText(methodName, src)
				if strings.HasPrefix(mName, "#") || mName == "constructor" {
					continue
				}
				isPrivate := false
				for k := range member.ChildCount() {
					c := member.Child(k)
					if c.Kind() == "accessibility_modifier" && nodeText(c, src) == "private" {
						isPrivate = true
						break
					}
				}
				mRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
				mRels = append(mRels, collectCalls(member, src, dir, symbolName, ctx.importMap)...)
				result = append(result, facts.Fact{
					Kind: facts.KindSymbol,
					Name: dir + "." + symbolName + "." + mName,
					File: relFile,
					Line: int(member.StartPosition().Row) + 1,
					Props: map[string]any{
						"symbol_kind": facts.SymbolMethod,
						"exported":    isExported && !isPrivate,
						"language":    "typescript",
						"receiver":    symbolName,
					},
					Relations: mRels,
				})
			}
		}

	case "interface_declaration":
		if name := findChildByKind(node, "type_identifier"); name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolInterface, isExported))
		}

	case "type_alias_declaration":
		if name := findChildByKind(node, "type_identifier"); name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolType, isExported))
		}

	case "enum_declaration":
		name := findChildByKind(node, "identifier")
		if name == nil {
			name = findChildByKind(node, "type_identifier")
		}
		if name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), facts.SymbolEnum, isExported))
		}

	case "internal_module", "module":
		// TypeScript `namespace X {}` / `module X {}`.
		name := findChildByKind(node, "identifier")
		if name == nil {
			name = findChildByKind(node, "nested_identifier")
		}
		if name != nil {
			result = append(result, e.simpleSymbol(node, ctx, nodeText(name, src), "namespace", isExported))
		}

	case "lexical_declaration", "variable_declaration":
		for j := range node.ChildCount() {
			decl := node.Child(j)
			if decl.Kind() != "variable_declarator" {
				continue
			}
			name := findChildByKind(decl, "identifier")
			if name == nil {
				continue
			}
			symbolName := nodeText(name, src)

			// Determine the value node and the symbol kind. Arrow functions and
			// memo/forwardRef-wrapped values are functions/components; everything
			// else is a plain variable.
			symbolKind := facts.SymbolVariable
			var body *sitter.Node
			if v := findChildByKind(decl, "arrow_function"); v != nil {
				symbolKind = facts.SymbolFunc
				body = v
			} else if call := findChildByKind(decl, "call_expression"); call != nil && isComponentWrapper(call, src) {
				symbolKind = facts.SymbolFunc
				body = call
			}

			vRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
			if body != nil {
				vRels = append(vRels, collectCalls(body, src, dir, "", ctx.importMap)...)
			}
			f := facts.Fact{
				Kind: facts.KindSymbol,
				Name: dir + "." + symbolName,
				File: relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					"symbol_kind": symbolKind,
					"exported":    isExported,
					"language":    "typescript",
				},
				Relations: vRels,
			}
			classifySymbol(&f, symbolName, body, ctx, symbolKind)
			result = append(result, f)
		}
	}

	return result
}

// funcSymbol builds a function/component symbol fact. declNode supplies the source
// location; body is walked for outgoing calls and JSX-based classification.
func (e *TSExtractor) funcSymbol(declNode, body *sitter.Node, ctx *extractCtx, name string, exported bool) facts.Fact {
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}}
	rels = append(rels, collectCalls(body, ctx.src, ctx.dir, "", ctx.importMap)...)
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: ctx.dir + "." + name,
		File: ctx.relFile,
		Line: int(declNode.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolFunc,
			"exported":    exported,
			"language":    "typescript",
		},
		Relations: rels,
	}
	classifySymbol(&f, name, body, ctx, facts.SymbolFunc)
	return f
}

// simpleSymbol builds a declaration-only symbol fact (interface, type, enum, namespace).
func (e *TSExtractor) simpleSymbol(node *sitter.Node, ctx *extractCtx, name, kind string, exported bool) facts.Fact {
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: ctx.dir + "." + name,
		File: ctx.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "typescript",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: ctx.dir}},
	}
	return f
}

// detectRoute checks if a file path corresponds to a Next.js route.
func detectRoute(relFile string) *facts.Fact {
	// Next.js App Router: app/**/page.tsx, app/**/route.tsx
	// Next.js Pages Router: pages/**/*.tsx

	parts := strings.Split(filepath.ToSlash(relFile), "/")

	// App Router
	for i, p := range parts {
		if p == "app" && i < len(parts)-1 {
			fileName := parts[len(parts)-1]
			baseName := strings.TrimSuffix(strings.TrimSuffix(fileName, ".tsx"), ".ts")

			if baseName == "page" || baseName == "route" || baseName == "layout" || baseName == "loading" || baseName == "error" {
				// Strip Next.js route groups — directory segments wrapped in ()
				// that act as layout organizers without affecting the URL.
				// e.g. (standard), (client-data), (header) → removed from path.
				segParts := parts[i+1 : len(parts)-1]
				urlParts := make([]string, 0, len(segParts))
				for _, seg := range segParts {
					if len(seg) >= 2 && seg[0] == '(' && seg[len(seg)-1] == ')' {
						continue // route group — not part of the URL
					}
					urlParts = append(urlParts, seg)
				}

				routePath := "/" + strings.Join(urlParts, "/")
				if routePath == "/" {
					routePath = "/"
				}

				method := "GET"
				if baseName == "route" {
					method = "ALL" // API route handler
				}

				return &facts.Fact{
					Kind: facts.KindRoute,
					Name: routePath,
					File: relFile,
					Line: 1,
					Props: map[string]any{
						"method":    method,
						"type":      baseName,
						"router":    "app",
						"language":  "typescript",
						"framework": "nextjs",
					},
				}
			}
		}
	}

	// Pages Router
	for i, p := range parts {
		if p == "pages" && i < len(parts)-1 {
			remaining := parts[i+1:]
			fileName := remaining[len(remaining)-1]
			baseName := strings.TrimSuffix(strings.TrimSuffix(fileName, ".tsx"), ".ts")

			// Skip _app, _document, _error
			if strings.HasPrefix(baseName, "_") {
				return nil
			}

			routeParts := make([]string, 0, len(remaining))
			for j, rp := range remaining {
				if j == len(remaining)-1 {
					if baseName != "index" {
						routeParts = append(routeParts, baseName)
					}
				} else {
					routeParts = append(routeParts, rp)
				}
			}

			routePath := "/" + strings.Join(routeParts, "/")

			// Detect API routes
			isAPI := len(remaining) > 0 && remaining[0] == "api"
			method := "GET"
			if isAPI {
				method = "ALL"
			}

			return &facts.Fact{
				Kind: facts.KindRoute,
				Name: routePath,
				File: relFile,
				Line: 1,
				Props: map[string]any{
					"method":    method,
					"type":      "page",
					"router":    "pages",
					"language":  "typescript",
					"framework": "nextjs",
				},
			}
		}
	}

	return nil
}

// detectNextJS checks if the repository is a Next.js project.
// It searches the TypeScript root directory (which may be a subdirectory in a
// monorepo) for next.config.* files or a package.json with a "next" dependency.
func detectNextJS(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	return detectNextJSAt(tsRoot) || (tsRoot != repoPath && detectNextJSAt(repoPath))
}

func detectNextJSAt(dir string) bool {
	// Check next.config.* at this directory level
	for _, name := range []string{"next.config.js", "next.config.mjs", "next.config.ts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}

	// Check package.json for next dependency
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := pkg[key].(map[string]any); ok {
			if _, ok := deps["next"]; ok {
				return true
			}
		}
	}
	return false
}

func isTypeScriptFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".ts" || ext == ".tsx"
}

// hasChildKind reports whether node has a direct child of the given kind.
func hasChildKind(node *sitter.Node, kind string) bool {
	return findChildByKind(node, kind) != nil
}

// firstDeclChild returns the first named declaration child of an export_statement,
// or nil if the export wraps something else (a value, re-export clause, etc.).
func firstDeclChild(node *sitter.Node) *sitter.Node {
	for _, k := range []string{
		"function_declaration", "generator_function_declaration",
		"class_declaration", "abstract_class_declaration",
		"interface_declaration", "type_alias_declaration",
		"lexical_declaration", "variable_declaration",
		"enum_declaration", "internal_module", "module",
	} {
		if c := findChildByKind(node, k); c != nil {
			return c
		}
	}
	return nil
}

// fileSymbolName derives a symbol name from a file path for anonymous default
// exports. Generic Next.js filenames (page, route, layout, …) are disambiguated
// with their parent directory segment, e.g. app/dashboard/page.tsx → "DashboardPage".
func fileSymbolName(relFile string) string {
	base := filepath.Base(relFile)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	switch base {
	case "index", "page", "route", "layout", "loading", "error", "not-found", "template", "default":
		parent := filepath.Base(filepath.Dir(relFile))
		if parent != "" && parent != "." && parent != string(filepath.Separator) {
			return toPascal(parent) + toPascal(base)
		}
	}
	return toPascal(base)
}

// toPascal converts an arbitrary identifier-ish string into PascalCase, splitting
// on any non-alphanumeric characters (e.g. "my-component" → "MyComponent").
func toPascal(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if upNext && r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upNext = false
		default:
			upNext = true
		}
	}
	return b.String()
}

// collectExportedLocalNames returns the set of locally-declared names that are
// exported via a separate `export { A, B as C }` clause or `export default Name`
// statement (where the declaration itself carries no inline export keyword).
func collectExportedLocalNames(root *sitter.Node, src []byte) map[string]bool {
	out := make(map[string]bool)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if child.Kind() != "export_statement" {
			continue
		}
		// export { A, B as C }
		if clause := findChildByKind(child, "export_clause"); clause != nil {
			for j := range clause.ChildCount() {
				spec := clause.Child(j)
				if spec.Kind() != "export_specifier" {
					continue
				}
				if n := spec.ChildByFieldName("name"); n != nil {
					out[nodeText(n, src)] = true
				}
			}
			continue
		}
		// export default Name
		if hasChildKind(child, "default") {
			if id := findChildByKind(child, "identifier"); id != nil {
				out[nodeText(id, src)] = true
			}
		}
	}
	return out
}

// reactHTTPMethods are the App Router route-handler export names.
var reactHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true,
}

// classifySymbol enriches a symbol fact with React/Next.js semantic props
// (web_component, framework, and for route handlers method), mirroring the
// ios_component/framework classification used by the Swift extractor. body, when
// non-nil, is scanned for JSX to confirm component-ness in non-TSX files.
func classifySymbol(f *facts.Fact, name string, body *sitter.Node, ctx *extractCtx, symbolKind string) {
	// Next.js App Router route handler: GET/POST/... in a route.{ts,tsx} file.
	if symbolKind == facts.SymbolFunc && reactHTTPMethods[name] && isAppRouteFile(ctx.relFile) {
		f.Props["web_component"] = "route_handler"
		f.Props["method"] = name
		f.Props["framework"] = "nextjs"
		return
	}
	// React hook: a useXxx function.
	if symbolKind == facts.SymbolFunc && isHookName(name) {
		f.Props["web_component"] = "hook"
		f.Props["framework"] = "react"
		return
	}
	// React component: a PascalCase function/class that renders JSX. In .tsx/.jsx
	// files a PascalCase function/class is treated as a component; elsewhere we
	// require literal JSX in the body to avoid misclassifying plain classes.
	if isComponentName(name) && (symbolKind == facts.SymbolFunc || symbolKind == facts.SymbolClass) {
		if ctx.isTSX || (body != nil && containsJSX(body)) {
			f.Props["web_component"] = "component"
			if ctx.isNextJS {
				f.Props["framework"] = "nextjs"
			} else {
				f.Props["framework"] = "react"
			}
		}
	}
}

// isHookName reports whether name follows the React hook convention useXxx.
func isHookName(name string) bool {
	if !strings.HasPrefix(name, "use") || len(name) < 4 {
		return false
	}
	c := name[3]
	return c >= 'A' && c <= 'Z'
}

// isComponentName reports whether name is PascalCase (a React component convention).
func isComponentName(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

// isAppRouteFile reports whether relFile is a Next.js App Router route handler
// file (a route.{ts,tsx} under an "app" directory segment).
func isAppRouteFile(relFile string) bool {
	base := filepath.Base(relFile)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".tsx"), ".ts")
	if base != "route" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(relFile), "/") {
		if seg == "app" {
			return true
		}
	}
	return false
}

// containsJSX reports whether the subtree rooted at node contains a JSX element.
func containsJSX(node *sitter.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind() {
	case "jsx_element", "jsx_self_closing_element", "jsx_fragment":
		return true
	}
	for i := range node.ChildCount() {
		if containsJSX(node.Child(i)) {
			return true
		}
	}
	return false
}

// isComponentWrapper reports whether a call expression wraps a component, i.e. it
// calls memo / forwardRef (optionally as React.memo / React.forwardRef).
func isComponentWrapper(call *sitter.Node, src []byte) bool {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return false
	}
	name := ""
	switch fn.Kind() {
	case "identifier":
		name = nodeText(fn, src)
	case "member_expression":
		if prop := fn.ChildByFieldName("property"); prop != nil {
			name = nodeText(prop, src)
		}
	}
	return name == "memo" || name == "forwardRef"
}

func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child.Kind() == kind {
			return child
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string {
	return string(src[node.StartByte():node.EndByte()])
}

// parseTSPathAliases reads tsconfig.json (or tsconfig.base.json for Nx monorepos)
// and extracts path alias mappings. For example "@/*": ["./src/*"] maps prefix
// "@/" to replacement "src/". It searches the TypeScript root directory first
// to support monorepos where the tsconfig lives in a subdirectory.
func parseTSPathAliases(repoPath string) map[string]string {
	tsRoot, _ := findTSRoot(repoPath)

	// Prefer tsconfig.json; fall back to tsconfig.base.json (Nx monorepo pattern).
	for _, name := range []string{"tsconfig.json", "tsconfig.base.json"} {
		if aliases, ok := tryParseTSConfigAliases(filepath.Join(tsRoot, name)); ok {
			return aliases
		}
	}
	// Also try the original repoPath if tsRoot is different.
	if tsRoot != repoPath {
		for _, name := range []string{"tsconfig.json", "tsconfig.base.json"} {
			if aliases, ok := tryParseTSConfigAliases(filepath.Join(repoPath, name)); ok {
				return aliases
			}
		}
	}
	return make(map[string]string)
}

func tryParseTSConfigAliases(tsconfigPath string) (map[string]string, bool) {
	data, err := os.ReadFile(tsconfigPath)
	if err != nil {
		return nil, false
	}

	var config struct {
		CompilerOptions struct {
			Paths map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, false
	}

	aliases := make(map[string]string)
	for pattern, targets := range config.CompilerOptions.Paths {
		if len(targets) == 0 {
			continue
		}
		// "@/*": ["./src/*"] → prefix "@/" maps to replacement "src/"
		if strings.HasSuffix(pattern, "*") && strings.HasSuffix(targets[0], "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			replacement := strings.TrimSuffix(targets[0], "*")
			replacement = strings.TrimPrefix(replacement, "./")
			aliases[prefix] = replacement
		}
	}
	return aliases, true
}

// resolveImportPath normalizes a TypeScript import path to a filesystem-relative path.
// It handles path aliases (@/), relative imports (./), and identifies external packages.
func resolveImportPath(importPath, fileDir string, aliases map[string]string) (string, bool) {
	// Try alias resolution first
	for prefix, replacement := range aliases {
		if strings.HasPrefix(importPath, prefix) {
			rest := strings.TrimPrefix(importPath, prefix)
			return filepath.ToSlash(filepath.Clean(replacement + rest)), false
		}
	}

	// Relative imports
	if strings.HasPrefix(importPath, ".") {
		resolved := filepath.ToSlash(filepath.Clean(filepath.Join(fileDir, importPath)))
		return resolved, false
	}

	// Everything else is external (react, next/image, @tanstack/react-query, etc.)
	return importPath, true
}

// buildImportSymbols returns a map of locally-bound import name → canonical symbol
// fact name for named imports from internal modules. It lets bare calls to
// imported functions (e.g. `formatName()`) resolve to the callee's declaration
// fact. Symbols declared in an imported module are named "<moduleDir>.<exportName>",
// where moduleDir is the directory of the resolved module file — this matches the
// common file-module case (e.g. import "./utils" → utils.ts → "<dir>.foo").
func buildImportSymbols(root *sitter.Node, src []byte, relFile string, aliases map[string]string) map[string]string {
	fileDir := filepath.Dir(relFile)
	m := make(map[string]string)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if child.Kind() != "import_statement" {
			continue
		}
		source := findChildByKind(child, "string")
		if source == nil {
			continue
		}
		importPath := strings.Trim(nodeText(source, src), `"'`)
		resolved, isExternal := resolveImportPath(importPath, fileDir, aliases)
		if isExternal {
			continue // external modules have no local declaration facts
		}
		moduleDir := filepath.Dir(resolved)

		clause := findChildByKind(child, "import_clause")
		if clause == nil {
			continue
		}
		named := findChildByKind(clause, "named_imports")
		if named == nil {
			continue // default/namespace imports are not resolved
		}
		for j := range named.ChildCount() {
			spec := named.Child(j)
			if spec.Kind() != "import_specifier" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			exportName := nodeText(nameNode, src)
			local := exportName
			if aliasNode := spec.ChildByFieldName("alias"); aliasNode != nil {
				local = nodeText(aliasNode, src)
			}
			m[local] = moduleDir + "." + exportName
		}
	}
	return m
}

// collectCalls walks a function/method body subtree and returns deduplicated
// RelCalls relations for each resolvable call expression. className, when
// non-empty, enables resolution of `this.method()` to "<dir>.<className>.<method>".
func collectCalls(node *sitter.Node, src []byte, dir, className string, importMap map[string]string) []facts.Relation {
	var rels []facts.Relation
	seen := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Kind() == "call_expression" {
			if target := resolveTSCall(n, src, dir, className, importMap); target != "" && !seen[target] {
				seen[target] = true
				rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(node)
	return rels
}

// resolveTSCall resolves a single call_expression to a canonical target fact name,
// or "" when the call cannot be resolved (e.g. a method call on a value of unknown
// type). It resolves:
//   - bare calls `foo()` → imported symbol via importMap, else same-module "<dir>.foo"
//   - `this.method()` inside a class → "<dir>.<className>.method"
func resolveTSCall(call *sitter.Node, src []byte, dir, className string, importMap map[string]string) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Kind() {
	case "identifier":
		name := nodeText(fn, src)
		if target, ok := importMap[name]; ok {
			return target
		}
		return dir + "." + name
	case "member_expression":
		object := fn.ChildByFieldName("object")
		property := fn.ChildByFieldName("property")
		if object == nil || property == nil {
			return ""
		}
		// `this.method()` resolves within the enclosing class; other receivers
		// have an unknown type and are left unresolved to avoid dangling edges.
		if object.Kind() == "this" && className != "" {
			return dir + "." + className + "." + nodeText(property, src)
		}
	}
	return ""
}
