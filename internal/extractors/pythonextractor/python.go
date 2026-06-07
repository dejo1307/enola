package pythonextractor

import (
	"bufio"
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// PythonExtractor extracts architectural facts from Python source code using
// line-based regex parsing with indentation-based scope tracking.
type PythonExtractor struct{}

// New creates a new PythonExtractor.
func New() *PythonExtractor {
	return &PythonExtractor{}
}

func (e *PythonExtractor) Name() string {
	return "python"
}

// Detect returns true if the repository looks like a Python project.
// It checks root-level markers first, then walks up to 3 subdirectory levels
// to support monorepos where Python code lives in a subdirectory (e.g. python/).
func (e *PythonExtractor) Detect(repoPath string) (bool, error) {
	// Root-level markers — fast path.
	rootMarkers := []string{
		"pyproject.toml", "setup.py", "requirements.txt", "Pipfile",
		"pytest.ini", "mypy.ini", "tox.ini", "setup.cfg",
	}
	for _, name := range rootMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, name)); err == nil {
			return true, nil
		}
	}

	// Subdirectory search (up to 3 levels deep) — handles monorepos.
	subMarkers := map[string]bool{
		"pyproject.toml": true,
		"setup.py":       true,
		"requirements.txt": true,
		"Pipfile":        true,
	}
	found := false
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, path)
		depth := strings.Count(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if depth >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if subMarkers[filepath.Base(path)] {
			found = true
		}
		return nil
	})
	return found, nil
}

// Extract parses Python files and emits architectural facts.
func (e *PythonExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact
	modules := make(map[string]bool)

	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isPythonFile(relFile) {
			continue
		}

		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			log.Printf("[python-extractor] error reading %s: %v", relFile, err)
			continue
		}

		fileFacts := extractFile(f, relFile)
		f.Close()
		allFacts = append(allFacts, fileFacts...)

		dir := filepath.Dir(relFile)
		modules[dir] = true
	}

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "python",
			},
		})
	}

	return allFacts, nil
}

// --- Regex patterns ---

var (
	// classRe matches class declarations. Groups: (indent, name, bases).
	classRe = regexp.MustCompile(`^(\s*)class\s+(\w+)\s*(?:\(([^)]*)\))?:`)

	// defRe matches function/method definitions. Groups: (indent, async, name).
	defRe = regexp.MustCompile(`^(\s*)(async\s+)?def\s+(\w+)\s*\(`)

	// importRe matches bare import statements. Group: (module).
	importRe = regexp.MustCompile(`^\s*import\s+([\w.]+)`)

	// fromImportRe matches from...import statements. Group: (module).
	fromImportRe = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\s+`)

	// routeDecoratorRe matches FastAPI/Starlette route decorators.
	// Groups: (object, http_method, path).
	routeDecoratorRe = regexp.MustCompile(`^\s*@([\w.]+)\.(get|post|put|delete|patch|head|options)\s*\(\s*["']([^"']+)["']`)

	// tableNameRe matches SQLAlchemy __tablename__ assignments. Group: (table).
	tableNameRe = regexp.MustCompile(`^\s*__tablename__\s*=\s*["']([^"']+)["']`)
)

// scopeEntry tracks a class nesting level with its indentation.
type scopeEntry struct {
	// qualifiedName is the fully-qualified class name (e.g. "dir.Outer.Inner").
	qualifiedName string
	// indent is the column indentation of the class keyword.
	indent int
}

// pendingRoute holds a FastAPI route decorator waiting for the handler def.
type pendingRoute struct {
	method string
	path   string
	line   int
}

// extractFile parses a single Python file and returns facts.
func extractFile(f *os.File, relFile string) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)
	// Python modules are file-based; strip .py to form the module prefix used in
	// symbol names (e.g. "app/models/order" for "app/models/order.py").
	// This avoids name collisions between classes in different files of the same
	// directory.
	module := strings.TrimSuffix(relFile, ".py")

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		lineNum        int
		scopeStack     []scopeEntry
		pendingRoutes  []pendingRoute
		inDocstring    bool
		docstringQuote string // `"""` or `'''`
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Handle multi-line docstrings / triple-quoted strings.
		if inDocstring {
			if strings.Contains(line, docstringQuote) {
				inDocstring = false
			}
			continue
		}

		// Detect opening of a triple-quoted string. We check after the inDocstring
		// block so that a line opening and closing on the same line is handled.
		if q, opens := opensTripleQuote(trimmed); opens {
			// Count occurrences: if odd number of the quote on this line, we enter
			// docstring mode for subsequent lines.
			if !closesOnSameLine(trimmed, q) {
				inDocstring = true
				docstringQuote = q
			}
			// The line itself is not a declaration, so we can skip to the next line.
			// (Triple-quote lines are never class/def/import lines.)
			continue
		}

		// Skip blank lines and comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Determine current line indentation.
		indent := lineIndent(line)

		// Pop scope entries that are at the same or deeper indentation level.
		// This handles returning to outer scope when indentation decreases.
		scopeStack = popScopes(scopeStack, indent)

		// Class declaration.
		if m := classRe.FindStringSubmatch(line); m != nil {
			// m[1]=indent, m[2]=name, m[3]=bases (may be empty)
			name := m[2]
			basesStr := strings.TrimSpace(m[3])

			qualName := buildQualName(module, scopeStack, name)

			props := map[string]any{
				"symbol_kind": facts.SymbolClass,
				"language":    "python",
			}

			rels := []facts.Relation{
				{Kind: facts.RelDeclares, Target: dir},
			}

			// Emit RelImplements for each base class.
			if basesStr != "" {
				for _, base := range splitBases(basesStr) {
					if base != "" {
						rels = append(rels, facts.Relation{
							Kind:   facts.RelImplements,
							Target: base,
						})
					}
				}
			}

			result = append(result, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      qualName,
				File:      relFile,
				Line:      lineNum,
				Props:     props,
				Relations: rels,
			})

			// Push to scope stack so nested members use this class as context.
			scopeStack = append(scopeStack, scopeEntry{
				qualifiedName: qualName,
				indent:        indent,
			})

			// A class declaration closes any pending route (decorators above a class
			// are not route handlers).
			pendingRoutes = nil
			continue
		}

		// Function / method definition.
		if m := defRe.FindStringSubmatch(line); m != nil {
			// m[1]=indent, m[2]=async (may be empty), m[3]=name
			isAsync := strings.TrimSpace(m[2]) == "async"
			funcName := m[3]

			var fullName string
			var symbolKind string

			if len(scopeStack) > 0 {
				// We are inside a class — this is a method.
				fullName = scopeStack[len(scopeStack)-1].qualifiedName + "." + funcName
				symbolKind = facts.SymbolMethod
			} else {
				// Top-level function.
				fullName = module + "." + funcName
				symbolKind = facts.SymbolFunc
			}

			props := map[string]any{
				"symbol_kind": symbolKind,
				"language":    "python",
			}
			if isAsync {
				props["async"] = true
			}

			fact := facts.Fact{
				Kind:  facts.KindSymbol,
				Name:  fullName,
				File:  relFile,
				Line:  lineNum,
				Props: props,
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			}

			// If there are pending route decorators, emit route facts now.
			for _, pr := range pendingRoutes {
				result = append(result, facts.Fact{
					Kind: facts.KindRoute,
					Name: pr.method + " " + pr.path,
					File: relFile,
					Line: pr.line,
					Props: map[string]any{
						"http_method": pr.method,
						"path":        pr.path,
						"handler":     fullName,
						"framework":   "fastapi",
						"language":    "python",
					},
				})
			}
			pendingRoutes = nil

			result = append(result, fact)
			continue
		}

		// Route decorator (@router.get("/path"), @app.post("/path"), etc.).
		if m := routeDecoratorRe.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[2])
			path := m[3]
			pendingRoutes = append(pendingRoutes, pendingRoute{
				method: method,
				path:   path,
				line:   lineNum,
			})
			continue
		}

		// Non-route decorator — keep any pending routes (multiple decorators on
		// the same function are allowed), but don't reset them here.
		if strings.HasPrefix(trimmed, "@") {
			continue
		}

		// Any non-decorator, non-def line clears pending routes.
		pendingRoutes = nil

		// Import: `import foo.bar`
		if m := importRe.FindStringSubmatch(line); m != nil {
			importPath := m[1]
			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: module + " -> " + importPath,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language": "python",
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: importPath},
				},
			})
			continue
		}

		// Import: `from foo.bar import ...`
		if m := fromImportRe.FindStringSubmatch(line); m != nil {
			importPath := m[1]
			result = append(result, facts.Fact{
				Kind: facts.KindDependency,
				Name: module + " -> " + importPath,
				File: relFile,
				Line: lineNum,
				Props: map[string]any{
					"language": "python",
					"from":     true,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: importPath},
				},
			})
			continue
		}

		// SQLAlchemy table name: `__tablename__ = "tbl"`
		if m := tableNameRe.FindStringSubmatch(line); m != nil {
			tableName := m[1]

			// Determine the owning class from the scope stack.
			ownerClass := ""
			if len(scopeStack) > 0 {
				ownerClass = scopeStack[len(scopeStack)-1].qualifiedName
			}

			props := map[string]any{
				"storage_kind": "table",
				"framework":    "sqlalchemy",
				"language":     "python",
			}
			if ownerClass != "" {
				props["class"] = ownerClass
			}

			result = append(result, facts.Fact{
				Kind:  facts.KindStorage,
				Name:  tableName,
				File:  relFile,
				Line:  lineNum,
				Props: props,
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})
			continue
		}
	}

	return result
}

// --- Helpers ---

// buildQualName constructs a qualified name like "module.Outer.Inner.Name".
// module is the file-based module path (e.g. "app/models/order" for "app/models/order.py").
func buildQualName(module string, stack []scopeEntry, name string) string {
	if len(stack) == 0 {
		return module + "." + name
	}
	return stack[len(stack)-1].qualifiedName + "." + name
}

// popScopes removes scope entries at or deeper than the given indentation.
func popScopes(stack []scopeEntry, indent int) []scopeEntry {
	for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
		stack = stack[:len(stack)-1]
	}
	return stack
}

// lineIndent returns the number of leading spaces in a line.
func lineIndent(line string) int {
	count := 0
	for _, ch := range line {
		if ch == ' ' {
			count++
		} else if ch == '\t' {
			count += 4 // treat tab as 4 spaces
		} else {
			break
		}
	}
	return count
}

// opensTripleQuote checks if a trimmed line starts a triple-quoted string and
// returns the quote style (`"""` or `'''`) and whether it opens one.
func opensTripleQuote(trimmed string) (string, bool) {
	for _, q := range []string{`"""`, `'''`} {
		if strings.Contains(trimmed, q) {
			return q, true
		}
	}
	return "", false
}

// closesOnSameLine returns true if the triple quote appears an even number of
// times on the line (opened and closed on the same line).
func closesOnSameLine(trimmed, q string) bool {
	count := strings.Count(trimmed, q)
	return count >= 2
}

// splitBases splits a Python base class list by comma, respecting bracket nesting
// so that generic types like `Generic[T]` or `CRUDBase[Model, Schema]` are kept
// as a single token.
func splitBases(s string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case ',':
			if depth == 0 {
				if t := strings.TrimSpace(s[start:i]); t != "" {
					result = append(result, stripGeneric(t))
				}
				start = i + 1
			}
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		result = append(result, stripGeneric(t))
	}
	return result
}

// stripGeneric removes generic type parameters from a base class name.
// e.g. "Generic[T]" → "Generic", "CRUDBase[Model, Schema]" → "CRUDBase".
func stripGeneric(s string) string {
	if idx := strings.Index(s, "["); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	if idx := strings.Index(s, "("); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

// isPythonFile returns true if the file has a .py extension.
func isPythonFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".py")
}
