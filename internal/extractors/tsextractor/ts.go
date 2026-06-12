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

func (e *TSExtractor) extractFile(src []byte, relFile string, isNextJS bool, aliases map[string]string) []facts.Fact {
	var result []facts.Fact

	// Parse openapi-typescript generated files for backend API route dependencies.
	// These are client-role route facts showing which backend routes the TS code calls.
	if openapiRoutes := extractOpenAPITypescriptFacts(src, relFile); len(openapiRoutes) > 0 {
		result = append(result, openapiRoutes...)
	}

	// Hand-written fetch / makeRequest API calls are also client-role routes.
	result = append(result, extractHTTPClientFacts(src, relFile)...)

	lang := typescript.LanguageTypescript()
	if strings.HasSuffix(relFile, ".tsx") {
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
	importMap := buildImportSymbols(root, src, relFile, aliases)
	result = append(result, e.extractDeclarations(root, src, relFile, importMap)...)

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

func (e *TSExtractor) extractDeclarations(root *sitter.Node, src []byte, relFile string, importMap map[string]string) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)

	for i := range root.ChildCount() {
		child := root.Child(i)
		ff := e.extractNode(child, src, relFile, dir, false, importMap)
		result = append(result, ff...)
	}

	return result
}

func (e *TSExtractor) extractNode(node *sitter.Node, src []byte, relFile, dir string, isExported bool, importMap map[string]string) []facts.Fact {
	var result []facts.Fact

	switch node.Kind() {
	case "export_statement":
		// Process the declaration inside the export
		decl := findChildByKind(node, "function_declaration")
		if decl == nil {
			decl = findChildByKind(node, "class_declaration")
		}
		if decl == nil {
			decl = findChildByKind(node, "interface_declaration")
		}
		if decl == nil {
			decl = findChildByKind(node, "type_alias_declaration")
		}
		if decl == nil {
			decl = findChildByKind(node, "lexical_declaration")
		}
		if decl != nil {
			return e.extractNode(decl, src, relFile, dir, true, importMap)
		}

	case "function_declaration":
		name := findChildByKind(node, "identifier")
		if name != nil {
			symbolName := nodeText(name, src)
			rels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
			rels = append(rels, collectCalls(node, src, dir, "", importMap)...)
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: dir + "." + symbolName,
				File: relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					"symbol_kind": facts.SymbolFunc,
					"exported":    isExported,
					"language":    "typescript",
				},
				Relations: rels,
			})
		}

	case "class_declaration":
		name := findChildByKind(node, "type_identifier")
		if name != nil {
			symbolName := nodeText(name, src)
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

			result = append(result, f)

			// Extract class methods
			classBody := findChildByKind(node, "class_body")
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
					mRels = append(mRels, collectCalls(member, src, dir, symbolName, importMap)...)
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
		}

	case "interface_declaration":
		name := findChildByKind(node, "type_identifier")
		if name != nil {
			symbolName := nodeText(name, src)
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: dir + "." + symbolName,
				File: relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					"symbol_kind": facts.SymbolInterface,
					"exported":    isExported,
					"language":    "typescript",
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})
		}

	case "type_alias_declaration":
		name := findChildByKind(node, "type_identifier")
		if name != nil {
			symbolName := nodeText(name, src)
			result = append(result, facts.Fact{
				Kind: facts.KindSymbol,
				Name: dir + "." + symbolName,
				File: relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					"symbol_kind": facts.SymbolType,
					"exported":    isExported,
					"language":    "typescript",
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})
		}

	case "lexical_declaration":
		// const/let/var declarations
		for j := range node.ChildCount() {
			decl := node.Child(j)
			if decl.Kind() == "variable_declarator" {
				name := findChildByKind(decl, "identifier")
				if name != nil {
					symbolName := nodeText(name, src)
					// Check if the value is an arrow function
					symbolKind := facts.SymbolVariable
					value := findChildByKind(decl, "arrow_function")
					if value != nil {
						symbolKind = facts.SymbolFunc
					}

					vRels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
					if value != nil {
						vRels = append(vRels, collectCalls(value, src, dir, "", importMap)...)
					}
					result = append(result, facts.Fact{
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
					})
				}
			}
		}
	}

	return result
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
