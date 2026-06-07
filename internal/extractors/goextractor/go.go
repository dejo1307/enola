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

	"github.com/enola-labs/enola/internal/facts"
)

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

// Extract parses Go files and emits architectural facts.
func (e *GoExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact
	fset := token.NewFileSet()
	modulePath := readModulePath(repoPath)

	// Group files by directory (package)
	packages := make(map[string][]string)
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		dir := filepath.Dir(f)
		packages[dir] = append(packages[dir], f)
	}

	for pkgDir, pkgFiles := range packages {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		pkgFacts := e.extractPackage(fset, repoPath, pkgDir, pkgFiles, modulePath)
		allFacts = append(allFacts, pkgFacts...)
	}

	return allFacts, nil
}

func (e *GoExtractor) extractPackage(fset *token.FileSet, repoPath, pkgDir string, files []string, modulePath string) []facts.Fact {
	var result []facts.Fact
	var pkgName string

	for _, relFile := range files {
		absFile := filepath.Join(repoPath, relFile)
		src, err := os.ReadFile(absFile)
		if err != nil {
			log.Printf("[go-extractor] error reading %s: %v", relFile, err)
			continue
		}

		f, err := parser.ParseFile(fset, absFile, src, parser.ParseComments)
		if err != nil {
			log.Printf("[go-extractor] error parsing %s: %v", relFile, err)
			continue
		}

		if pkgName == "" {
			pkgName = f.Name.Name
		}

		fileFacts := e.extractFile(fset, f, relFile, pkgDir, modulePath)
		result = append(result, fileFacts...)
	}

	// Emit module fact for the package
	if pkgName != "" {
		moduleFact := facts.Fact{
			Kind: facts.KindModule,
			Name: pkgDir,
			File: pkgDir,
			Props: map[string]any{
				"package":  pkgName,
				"language": "go",
			},
		}
		result = append(result, moduleFact)
	}

	return result
}

func (e *GoExtractor) extractFile(fset *token.FileSet, f *ast.File, relFile, pkgDir, modulePath string) []facts.Fact {
	var result []facts.Fact

	// Extract imports
	for _, imp := range f.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)

		// Normalize internal import targets to short paths (e.g.
		// "github.com/foo/bar/internal/pkg" → "internal/pkg") so they
		// match the module fact names used elsewhere in the store.
		relTarget := importPath
		if modulePath != "" && strings.HasPrefix(importPath, modulePath+"/") {
			relTarget = strings.TrimPrefix(importPath, modulePath+"/")
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
			result = append(result, e.extractFunc(fset, d, relFile, pkgDir)...)
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

func (e *GoExtractor) extractFunc(fset *token.FileSet, fn *ast.FuncDecl, relFile, pkgDir string) []facts.Fact {
	var result []facts.Fact

	name := fn.Name.Name
	exported := fn.Name.IsExported()
	kind := facts.SymbolFunc

	var receiver string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = facts.SymbolMethod
		receiver = typeExprToString(fn.Recv.List[0].Type)
		name = receiver + "." + name
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
		calls := extractCalls(fn.Body)
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

// extractCalls walks an AST node and extracts function call target names.
func extractCalls(node ast.Node) []string {
	var calls []string
	ast.Inspect(node, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fn := ce.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, fn.Name)
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok {
				calls = append(calls, x.Name+"."+fn.Sel.Name)
			}
		}
		return true
	})
	return calls
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
