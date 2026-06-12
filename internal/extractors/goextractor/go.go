package goextractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
)

// goBuiltins are Go's predeclared functions and type-conversion identifiers.
// Bare calls to these (e.g. len(x), make(...), string(b)) are not calls to a
// symbol, so resolving them would produce dangling phantom call edges.
var goBuiltins = map[string]bool{
	// Builtin functions.
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	// Predeclared types used as conversions.
	"string": true, "bool": true, "byte": true, "rune": true, "error": true,
	"any": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "uint": true, "uint8": true, "uint16": true, "uint32": true,
	"uint64": true, "uintptr": true, "float32": true, "float64": true,
	"complex64": true, "complex128": true,
}

// GoExtractor extracts architectural facts from Go source code using go/ast.
type GoExtractor struct{}

// New creates a new GoExtractor.
func New() *GoExtractor {
	return &GoExtractor{}
}

func (e *GoExtractor) Name() string {
	return "go"
}

// Detect returns true if the repository contains a go.mod file.
func (e *GoExtractor) Detect(repoPath string) (bool, error) {
	_, err := os.Stat(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// parsedPkg holds parsing results for a single Go package directory.
type parsedPkg struct {
	pkgName     string
	relFiles    []string
	parsedFiles []*ast.File
	fileMap     map[string]*ast.File // relFile → *ast.File
}

// Extract parses Go files and emits architectural facts.
// It uses three global passes so that struct field types are visible across
// package boundaries within the same module — necessary for resolving
// multi-hop call chains like h.authLib.Service.Register.
func (e *GoExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact
	fset := token.NewFileSet()
	modulePath := readModulePath(repoPath)

	// Pass 1: parse all Go files, grouping by package directory.
	parsedPkgs := make(map[string]*parsedPkg)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		pkgDir := filepath.Dir(f)
		pp := parsedPkgs[pkgDir]
		if pp == nil {
			pp = &parsedPkg{fileMap: make(map[string]*ast.File)}
			parsedPkgs[pkgDir] = pp
		}
		pp.relFiles = append(pp.relFiles, f)

		absFile := filepath.Join(repoPath, f)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[go-extractor] error reading %s: %v", f, err)
			continue
		}
		parsed, err := parser.ParseFile(fset, absFile, src, parser.ParseComments)
		if err != nil {
			log.Printf("[go-extractor] error parsing %s: %v", f, err)
			continue
		}
		if pp.pkgName == "" {
			pp.pkgName = parsed.Name.Name
		}
		pp.parsedFiles = append(pp.parsedFiles, parsed)
		pp.fileMap[f] = parsed
	}

	// Build declared package-name map so buildFileImports can resolve implicit
	// aliases correctly (e.g. "go-auth" path base → "auth" package name).
	pkgNames := make(map[string]string)
	for pkgDir, pp := range parsedPkgs {
		if pp.pkgName != "" {
			pkgNames[pkgDir] = pp.pkgName
		}
	}

	// Pass 2: build a global field-type map from ALL packages in the module.
	// This allows cross-package field-chain resolution (e.g., a subpackage can
	// look up fields of a root-package struct).
	globalFieldTypes := make(map[string]string)
	for pkgDir, pp := range parsedPkgs {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}
		for k, v := range collectFieldTypes(pp.parsedFiles, pkgDir, modulePath, pkgNames) {
			globalFieldTypes[k] = v
		}
	}

	// Pass 3: extract facts per package using the global field types.
	for pkgDir, pp := range parsedPkgs {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}
		if pp.pkgName == "" {
			continue
		}
		pkgFacts := e.extractPackage(fset, pkgDir, pp, modulePath, globalFieldTypes, pkgNames)
		allFacts = append(allFacts, pkgFacts...)
	}

	return allFacts, nil
}

func (e *GoExtractor) extractPackage(fset *token.FileSet, pkgDir string, pp *parsedPkg, modulePath string, fieldTypes map[string]string, pkgNames map[string]string) []facts.Fact {
	var result []facts.Fact

	for _, relFile := range pp.relFiles {
		f, ok := pp.fileMap[relFile]
		if !ok {
			continue
		}
		result = append(result, e.extractFile(fset, f, relFile, pkgDir, modulePath, fieldTypes, pkgNames)...)
	}

	moduleFact := facts.Fact{
		Kind: facts.KindModule,
		Name: pkgDir,
		File: pkgDir,
		Props: map[string]any{
			"package":  pp.pkgName,
			"language": "go",
		},
	}
	// Store the full Go module path on the root package fact so that the graph
	// layer can normalise cross-repo call targets (Bug 2).
	if pkgDir == "." && modulePath != "" {
		moduleFact.Props["modulePath"] = modulePath
	}
	result = append(result, moduleFact)

	return result
}

func (e *GoExtractor) extractFile(fset *token.FileSet, f *ast.File, relFile, pkgDir, modulePath string, fieldTypes map[string]string, pkgNames map[string]string) []facts.Fact {
	var result []facts.Fact

	// Build per-file import alias map for call resolution.
	fileImports := buildFileImports(f, modulePath, pkgNames)

	// Extract imports
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		// Normalize internal import targets to short paths (e.g.
		// "github.com/foo/bar/internal/pkg" → "internal/pkg") so they
		// match the module fact names used elsewhere in the store.
		relTarget := importPath
		if modulePath != "" {
			if importPath == modulePath {
				relTarget = "."
			} else if strings.HasPrefix(importPath, modulePath+"/") {
				relTarget = strings.TrimPrefix(importPath, modulePath+"/")
			}
		}

		result = append(result, facts.Fact{
			Kind: facts.KindDependency,
			Name: pkgDir + " -> " + importPath,
			File: relFile,
			Line: fset.Position(imp.Pos()).Line,
			Props: map[string]any{
				"language": "go",
				"source":   classifyImport(importPath, modulePath),
			},
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: relTarget},
			},
		})
	}

	// Walk declarations
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			result = append(result, e.extractFunc(fset, d, relFile, pkgDir, modulePath, fileImports, fieldTypes)...)
		case *ast.GenDecl:
			result = append(result, e.extractGenDecl(fset, d, relFile, pkgDir)...)
		}
	}

	// Extract route registrations
	result = append(result, extractRoutes(fset, f, relFile, pkgDir)...)

	// Extract storage patterns
	result = append(result, extractStorage(fset, f, relFile, pkgDir)...)

	return result
}

func (e *GoExtractor) extractFunc(fset *token.FileSet, fn *ast.FuncDecl, relFile, pkgDir, modulePath string, fileImports map[string]string, fieldTypes map[string]string) []facts.Fact {
	var result []facts.Fact

	name := fn.Name.Name
	exported := fn.Name.IsExported()
	kind := facts.SymbolFunc

	var receiver string
	var recvVar string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = facts.SymbolMethod
		field := fn.Recv.List[0]
		receiver = typeExprToString(field.Type)
		name = receiver + "." + name
		if len(field.Names) > 0 {
			recvVar = field.Names[0].Name
		}
	}

	qualifiedName := pkgDir + "." + name

	symbolFact := facts.Fact{
		Kind: facts.KindSymbol,
		Name: qualifiedName,
		File: relFile,
		Line: fset.Position(fn.Pos()).Line,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "go",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}

	if receiver != "" {
		symbolFact.Props["receiver"] = receiver
	}

	// Extract function calls
	if fn.Body != nil {
		ctx := resolveCtx{
			pkgDir:     pkgDir,
			modulePath: modulePath,
			imports:    fileImports,
			recvVar:    recvVar,
			recvType:   receiver,
			fieldTypes: fieldTypes,
		}
		ctx.localTypes = collectLocalTypes(fn.Body, ctx)
		calls := extractCalls(fn.Body, ctx)
		for _, call := range calls {
			symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: call,
			})
		}
	}

	result = append(result, symbolFact)
	return result
}

func (e *GoExtractor) extractGenDecl(fset *token.FileSet, gd *ast.GenDecl, relFile, pkgDir string) []facts.Fact {
	var result []facts.Fact

	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			result = append(result, e.extractTypeSpec(fset, gd, s, relFile, pkgDir)...)
		}
	}

	return result
}

func (e *GoExtractor) extractTypeSpec(fset *token.FileSet, gd *ast.GenDecl, ts *ast.TypeSpec, relFile, pkgDir string) []facts.Fact {
	var result []facts.Fact

	name := ts.Name.Name
	exported := ts.Name.IsExported()
	qualifiedName := pkgDir + "." + name

	var kind string
	var implements []string

	switch t := ts.Type.(type) {
	case *ast.StructType:
		kind = facts.SymbolStruct
		// Extract embedded types (potential interface implementations)
		if t.Fields != nil {
			for _, field := range t.Fields.List {
				if len(field.Names) == 0 {
					// Embedded type
					embeddedName := typeExprToString(field.Type)
					if embeddedName != "" {
						implements = append(implements, embeddedName)
					}
				}
			}
		}
	case *ast.InterfaceType:
		kind = facts.SymbolInterface
	default:
		kind = facts.SymbolType
	}

	symbolFact := facts.Fact{
		Kind: facts.KindSymbol,
		Name: qualifiedName,
		File: relFile,
		Line: fset.Position(ts.Pos()).Line,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "go",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: pkgDir},
		},
	}

	for _, impl := range implements {
		symbolFact.Relations = append(symbolFact.Relations, facts.Relation{
			Kind:   facts.RelImplements,
			Target: impl,
		})
	}

	result = append(result, symbolFact)
	return result
}

// resolveCtx holds the context needed to resolve call targets within a function body.
type resolveCtx struct {
	pkgDir     string
	modulePath string
	imports    map[string]string // alias → relative package path
	recvVar    string            // receiver variable name, e.g. "h"
	recvType   string            // receiver type (star stripped), e.g. "AuthHandler"
	fieldTypes map[string]string // "pkgDir.TypeName.fieldName" → pre-qualified typeString
	localTypes map[string]string // local variable name → qualified type, e.g. "svc" → "internal/auth.Service"
}

// extractCalls walks an AST node and extracts function call target names,
// resolving them to qualified fact names where possible.
func extractCalls(node ast.Node, ctx resolveCtx) []string {
	var calls []string
	seen := make(map[string]bool)
	ast.Inspect(node, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		chain := flattenSelector(ce.Fun)
		if chain == nil {
			return true
		}
		resolved := resolveChain(chain, ctx)
		if resolved != "" && !seen[resolved] {
			seen[resolved] = true
			calls = append(calls, resolved)
		}
		return true
	})
	return calls
}

// flattenSelector converts a (potentially deep) selector chain to a left-to-right
// slice of name segments. Returns nil for non-identifier/non-selector expressions
// (e.g. function-result calls, type assertions, index expressions).
func flattenSelector(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.Ident:
		return []string{e.Name}
	case *ast.SelectorExpr:
		prefix := flattenSelector(e.X)
		if prefix == nil {
			return nil
		}
		return append(prefix, e.Sel.Name)
	}
	return nil
}

// buildFileImports returns a map of import alias → relative package path for all
// imports in f. The relative path strips the module prefix to match fact naming.
// Exact-match self-imports (importPath == modulePath) map to "." (the root package).
// Blank ("_") and dot (".") imports are excluded.
// pkgNames maps pkgDir → declared package name (from parsing); it is used to
// resolve the implicit alias for packages whose path base is not a valid identifier
// (e.g. "github.com/x/go-auth" has base "go-auth" but package name "auth").
func buildFileImports(f *ast.File, modulePath string, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
		}

		relTarget := importPath
		if modulePath != "" {
			if importPath == modulePath {
				// Subpackage importing the module root — map to "." so that
				// call targets resolve to root-package fact names.
				relTarget = "."
			} else if strings.HasPrefix(importPath, modulePath+"/") {
				relTarget = strings.TrimPrefix(importPath, modulePath+"/")
			}
		}

		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		} else if pkgNames != nil {
			// Use the declared package name as alias when available — this is
			// correct when the last path segment isn't a valid identifier
			// (e.g. "go-auth" → package name "auth").
			if name, ok := pkgNames[relTarget]; ok {
				alias = name
			} else {
				alias = filepath.Base(importPath)
			}
		} else {
			alias = filepath.Base(importPath)
		}

		if alias != "" {
			m[alias] = relTarget
		}
	}
	return m
}

// collectFieldTypes pre-scans all struct declarations in the given parsed files
// and returns a map of "pkgDir.TypeName.fieldName" → pre-qualified typeString for
// named fields. Types are pre-qualified at collection time using each struct's
// source-package context so they remain correct when looked up from a different
// package (e.g. an adapters package looking up root-package struct fields).
func collectFieldTypes(files []*ast.File, pkgDir, modulePath string, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	for _, f := range files {
		fileImports := buildFileImports(f, modulePath, pkgNames)
		ctx := resolveCtx{pkgDir: pkgDir, imports: fileImports}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				typeName := ts.Name.Name
				for _, field := range st.Fields.List {
					typeStr := typeExprToString(field.Type)
					if typeStr == "" {
						continue
					}
					// Pre-qualify so cross-package lookups return the correct
					// fact-name prefix rather than the local alias or bare type name.
					qualifiedType := resolveTypeName(typeStr, ctx)
					for _, fname := range field.Names {
						key := pkgDir + "." + typeName + "." + fname.Name
						m[key] = qualifiedType
					}
				}
			}
		}
	}
	return m
}

// resolveChain resolves a flattened call chain to a graph fact name.
//
// Resolution rules:
//   - 1 element (bare call): same-package function → pkgDir.name
//   - 2 elements [alias, func]: import alias → relPath.func; receiver var → pkgDir.ReceiverType.func; fallback → raw join
//   - 3+ elements: resolve root to a qualified "pkg.Type", walk intermediate fields via fieldTypes, produce qualifiedType.method
//
// Falls back to the raw joined string when resolution is not possible, so no call is dropped.
//
// Known limitation: calls through an interface value (e.g. iface.Method()) cannot be
// statically bound to a concrete implementation without type-flow analysis, so the
// resolved target may name an interface method that has no backing symbol fact. Such
// edges surface as "unresolved" nodes during traversal rather than concrete callees.
func resolveChain(chain []string, ctx resolveCtx) string {
	switch len(chain) {
	case 0:
		return ""
	case 1:
		// Builtins and predeclared type conversions (len, make, string(...), etc.)
		// are not symbols — emitting them produces dangling phantom nodes.
		if goBuiltins[chain[0]] {
			return ""
		}
		return ctx.pkgDir + "." + chain[0]
	case 2:
		root, sel := chain[0], chain[1]
		if importPath, ok := ctx.imports[root]; ok {
			return importPath + "." + sel
		}
		if root == ctx.recvVar && ctx.recvType != "" {
			return ctx.pkgDir + "." + ctx.recvType + "." + sel
		}
		if qualType, ok := ctx.localTypes[root]; ok && qualType != "" {
			// root is a local variable of a known type; sel is a method on it.
			return qualType + "." + sel
		}
		return root + "." + sel
	default:
		// 3+ elements: attempt field-chain resolution.
		root := chain[0]
		var qualType string // "pkgDir.TypeName" or "importedPkg.TypeName"
		var fieldStart int  // index of the first intermediate field in chain

		if root == ctx.recvVar && ctx.recvType != "" {
			qualType = ctx.pkgDir + "." + ctx.recvType
			fieldStart = 1
		} else if lt, ok := ctx.localTypes[root]; ok && lt != "" {
			// root is a local variable; its type is already fully qualified.
			qualType = lt
			fieldStart = 1
		} else if importPath, ok := ctx.imports[root]; ok {
			// root is an import alias; chain[1] is a type in that package.
			qualType = importPath + "." + chain[1]
			fieldStart = 2
		} else {
			return strings.Join(chain, ".")
		}

		for _, fieldName := range chain[fieldStart : len(chain)-1] {
			key := qualType + "." + fieldName
			nextType, ok := ctx.fieldTypes[key]
			if !ok {
				return strings.Join(chain, ".")
			}
			qualType = resolveTypeName(nextType, ctx)
		}

		return qualType + "." + chain[len(chain)-1]
	}
}

// collectLocalTypes scans a function body for local variable declarations whose
// type is statically knowable and returns a map of variable name → qualified type
// name (e.g. "svc" → "internal/auth.Service"). It recognises:
//   - `var x T` / `var x *T` declarations
//   - `x := &Foo{}` / `x := Foo{}` composite literals
//   - `x := NewFoo(...)` / `x := pkg.NewFoo(...)` constructor conventions
//
// Variable names are recorded so that calls like `svc.Do()` resolve to the
// canonical method fact name instead of dangling on a raw join.
func collectLocalTypes(body ast.Node, ctx resolveCtx) map[string]string {
	locals := make(map[string]string)
	ast.Inspect(body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeclStmt:
			gd, ok := stmt.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					if typeStr := typeExprToString(vs.Type); typeStr != "" {
						qual := resolveTypeName(typeStr, ctx)
						for _, name := range vs.Names {
							if name.Name != "_" {
								locals[name.Name] = qual
							}
						}
					}
					continue
				}
				// `var x = <rhs>` — infer from the initializer.
				if len(vs.Names) == len(vs.Values) {
					for i, name := range vs.Names {
						if name.Name == "_" {
							continue
						}
						if qual := inferRHSType(vs.Values[i], ctx); qual != "" {
							locals[name.Name] = qual
						}
					}
				}
			}
		case *ast.AssignStmt:
			if stmt.Tok != token.DEFINE || len(stmt.Lhs) != len(stmt.Rhs) {
				return true
			}
			for i, lhs := range stmt.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				if qual := inferRHSType(stmt.Rhs[i], ctx); qual != "" {
					locals[ident.Name] = qual
				}
			}
		}
		return true
	})
	return locals
}

// inferRHSType attempts to determine the qualified type of an expression used as
// the right-hand side of a variable assignment. Returns "" when the type is not
// statically knowable.
func inferRHSType(expr ast.Expr, ctx resolveCtx) string {
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		// &Foo{}
		if cl, ok := e.X.(*ast.CompositeLit); ok {
			return compositeLitType(cl, ctx)
		}
	case *ast.CompositeLit:
		// Foo{} or pkg.Foo{}
		return compositeLitType(e, ctx)
	case *ast.CallExpr:
		// NewFoo(...) / pkg.NewFoo(...)
		return constructorReturnType(e.Fun, ctx)
	}
	return ""
}

// compositeLitType returns the qualified type name of a composite literal, or ""
// when the literal has no named type (e.g. slice/map literals).
func compositeLitType(cl *ast.CompositeLit, ctx resolveCtx) string {
	if cl.Type == nil {
		return ""
	}
	typeStr := typeExprToString(cl.Type)
	if typeStr == "" {
		return ""
	}
	return resolveTypeName(typeStr, ctx)
}

// constructorReturnType infers the qualified return type of a call following the
// `New<Type>` convention (e.g. `NewService()` → "pkgDir.Service",
// `auth.NewClient()` → "internal/auth.Client"). Returns "" otherwise.
func constructorReturnType(fun ast.Expr, ctx resolveCtx) string {
	switch f := fun.(type) {
	case *ast.Ident:
		if t := newConventionType(f.Name); t != "" {
			return ctx.pkgDir + "." + t
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			if t := newConventionType(f.Sel.Name); t != "" {
				if importPath, ok := ctx.imports[x.Name]; ok {
					return importPath + "." + t
				}
			}
		}
	}
	return ""
}

// newConventionType returns the type name implied by a constructor following the
// `New<Type>` convention (e.g. "NewService" → "Service"), or "" if the name does
// not follow it (e.g. "New", "Newton").
func newConventionType(name string) string {
	rest := strings.TrimPrefix(name, "New")
	if rest == name || rest == "" {
		return ""
	}
	if !unicode.IsUpper([]rune(rest)[0]) {
		return ""
	}
	return rest
}

// resolveTypeName converts a raw type string (e.g. "pkg.Type" or "LocalType")
// to a fully qualified "relPath.Type" form using the import alias map.
// Pre-qualified types (e.g. "..AuthService" stored by collectFieldTypes) are
// passed through unchanged when their alias part is not in the import map.
func resolveTypeName(typeStr string, ctx resolveCtx) string {
	if !strings.Contains(typeStr, ".") {
		return ctx.pkgDir + "." + typeStr
	}
	parts := strings.SplitN(typeStr, ".", 2)
	if resolvedPkg, ok := ctx.imports[parts[0]]; ok {
		return resolvedPkg + "." + parts[1]
	}
	return typeStr
}

// readModulePath reads the module path from go.mod in the given repo.
func readModulePath(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// classifyImport returns "stdlib", "internal", or "external" for a Go import path.
func classifyImport(importPath, modulePath string) string {
	// stdlib: first path segment has no dots
	firstSegment := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		firstSegment = importPath[:i]
	}
	if !strings.Contains(firstSegment, ".") {
		return "stdlib"
	}
	// internal: starts with the module path
	if modulePath != "" && (importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")) {
		return "internal"
	}
	return "external"
}

// typeExprToString converts a type expression to a string representation.
func typeExprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeExprToString(t.X)
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok {
			return x.Name + "." + t.Sel.Name
		}
	case *ast.IndexExpr:
		return typeExprToString(t.X)
	}
	return ""
}
