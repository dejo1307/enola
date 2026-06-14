package pythonextractor

import (
	"bufio"
	"context"
	"io"
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
		"pyproject.toml":   true,
		"setup.py":         true,
		"requirements.txt": true,
		"Pipfile":          true,
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
	isDjango := detectDjango(repoPath)

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

		src, readErr := readAll(f)
		f.Close()
		var fileFacts []facts.Fact
		if readErr != nil {
			log.Printf("[python-extractor] error reading %s: %v", relFile, readErr)
			continue
		}
		fileFacts = extractFileAST(src, relFile, isDjango)
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

	// decoratorRe captures the full decorator name for structural prop detection.
	// Group: (name) e.g. "staticmethod", "app.task".
	decoratorRe = regexp.MustCompile(`^\s*@([\w.]+)`)

	// returnTypeRe extracts return type from a single-line def signature.
	// e.g. "def foo(x: int) -> Optional[str]:"
	returnTypeRe = regexp.MustCompile(`\)\s*->\s*([\w\[\], |.]+?)\s*:`)

	// returnTypeClosingRe matches the closing paren of a multi-line def with
	// a return type annotation. e.g. "    ) -> Optional[str]:"
	returnTypeClosingRe = regexp.MustCompile(`^\s*\)\s*->\s*([\w\[\], |.]+?)\s*:`)

	// apiViewRe matches Django REST Framework @api_view decorators.
	// Group: (methods_list) — bracket contents, e.g. "'GET', 'POST'"
	apiViewRe = regexp.MustCompile(`^\s*@(?:[\w.]*\.)?api_view\s*\(\s*\[([^\]]+)\]`)

	// httpMethodWordRe extracts uppercase HTTP method tokens from an api_view list.
	httpMethodWordRe = regexp.MustCompile(`[A-Z]+`)

	// urlPathRe matches Django path() and re_path() calls in urls.py.
	// Groups: (url_path, view_ref)
	urlPathRe = regexp.MustCompile(`(?:re_)?path\s*\(\s*r?["']([^"']+)["']\s*,\s*([\w.]+)`)
)

// Django class base sets used to classify models, views, and serializers.
var (
	djangoModelBases = map[string]bool{
		"Model": true, "AbstractModel": true, "MPTTModel": true,
		"TimeStampedModel": true, "UUIDModel": true, "PolymorphicModel": true,
	}

	djangoCBVBases = map[string]bool{
		"View": true, "APIView": true, "GenericAPIView": true,
		"ListAPIView": true, "CreateAPIView": true, "RetrieveAPIView": true,
		"UpdateAPIView": true, "DestroyAPIView": true, "ListCreateAPIView": true,
		"RetrieveUpdateDestroyAPIView": true, "ViewSet": true, "ModelViewSet": true,
		"ReadOnlyModelViewSet": true, "TemplateView": true, "DetailView": true,
		"ListView": true, "CreateView": true, "UpdateView": true, "DeleteView": true,
		"FormView": true, "RedirectView": true,
	}

	djangoSerializerBases = map[string]bool{
		"Serializer": true, "ModelSerializer": true,
		"HyperlinkedModelSerializer": true, "ListSerializer": true,
	}
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
func extractFile(f *os.File, relFile string, isDjango bool) []facts.Fact {
	var result []facts.Fact
	dir := filepath.Dir(relFile)
	// Python modules are file-based; strip .py to form the module prefix used in
	// symbol names (e.g. "app/models/order" for "app/models/order.py").
	module := strings.TrimSuffix(relFile, ".py")
	isURLsFile := isDjango && filepath.Base(relFile) == "urls.py"

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var (
		lineNum               int
		scopeStack            []scopeEntry
		pendingRoutes         []pendingRoute
		pendingDecorators     []string       // decorator names since last def/class
		pendingApiViewMethods []string       // HTTP methods from @api_view
		decoratorParenDepth   int            // open-paren depth inside a multi-line decorator arg list
		pendingFuncProps      map[string]any // props of last emitted func (for return-type backfill)
		pendingFuncLine       int
		inDocstring           bool
		docstringQuote        string // `"""` or `'''`
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
		scopeStack = popScopes(scopeStack, indent)

		// Expire pending return-type backfill after 20 lines.
		if pendingFuncProps != nil && lineNum-pendingFuncLine > 20 {
			pendingFuncProps = nil
		}

		// Check for return type on the closing line of a multi-line function
		// signature, e.g. "    ) -> Optional[str]:". The props map is shared with
		// the already-emitted fact, so updating it here updates the fact in-place.
		if pendingFuncProps != nil {
			if rt := returnTypeClosingRe.FindStringSubmatch(line); rt != nil {
				pendingFuncProps["return_type"] = strings.TrimSpace(rt[1])
				pendingFuncProps = nil
			}
		}

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

			bases := splitBases(basesStr)

			// Emit RelImplements for each base class.
			for _, base := range bases {
				if base != "" {
					rels = append(rels, facts.Relation{
						Kind:   facts.RelImplements,
						Target: base,
					})
				}
			}

			// Django-specific class classification.
			if isDjango {
				for _, base := range bases {
					bn := lastComponent(base)
					if djangoModelBases[bn] {
						// Emit KindStorage for the Django-inferred table name.
						result = append(result, facts.Fact{
							Kind: facts.KindStorage,
							Name: camelToSnake(name),
							File: relFile,
							Line: lineNum,
							Props: map[string]any{
								"storage_kind": "table",
								"framework":    "django",
								"language":     "python",
								"class":        qualName,
							},
							Relations: []facts.Relation{
								{Kind: facts.RelDeclares, Target: dir},
							},
						})
						break
					}
					if djangoCBVBases[bn] {
						props["django_component"] = "view"
						props["framework"] = "django"
						break
					}
					if djangoSerializerBases[bn] {
						props["django_component"] = "serializer"
						props["framework"] = "django"
						break
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

			// A class declaration closes all pending state.
			pendingRoutes = nil
			pendingDecorators = nil
			pendingApiViewMethods = nil
			decoratorParenDepth = 0
			pendingFuncProps = nil
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

			// Apply structural props from pending decorators.
			for _, dec := range pendingDecorators {
				applyDecoratorProps(props, dec)
			}
			pendingDecorators = nil
			decoratorParenDepth = 0

			// Extract return type from the current line (single-line signature).
			if rt := returnTypeRe.FindStringSubmatch(line); rt != nil {
				props["return_type"] = strings.TrimSpace(rt[1])
				pendingFuncProps = nil
			} else {
				// Multi-line signature: backfill return type when closing line appears.
				pendingFuncProps = props
				pendingFuncLine = lineNum
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

			// Emit FastAPI route facts.
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

			// Emit Django @api_view route facts.
			for _, method := range pendingApiViewMethods {
				result = append(result, facts.Fact{
					Kind: facts.KindRoute,
					Name: method + " (view) " + fullName,
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"http_method": method,
						"path":        "",
						"handler":     fullName,
						"framework":   "django",
						"language":    "python",
					},
				})
			}
			pendingApiViewMethods = nil

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

		// Any decorator line (@...).
		if strings.HasPrefix(trimmed, "@") {
			// Django @api_view(['GET', 'POST']) — parse HTTP methods.
			if isDjango {
				if m := apiViewRe.FindStringSubmatch(line); m != nil {
					methods := httpMethodWordRe.FindAllString(m[1], -1)
					pendingApiViewMethods = append(pendingApiViewMethods, methods...)
					decoratorParenDepth = 0 // api_view always single-line
					continue
				}
			}

			// Capture decorator name for structural symbol props.
			if m := decoratorRe.FindStringSubmatch(line); m != nil {
				pendingDecorators = append(pendingDecorators, m[1])
			}
			// Track open parens so multi-line decorator args don't clear pending state.
			decoratorParenDepth = strings.Count(line, "(") - strings.Count(line, ")")
			if decoratorParenDepth < 0 {
				decoratorParenDepth = 0
			}
			continue
		}

		// Inside a multi-line decorator argument list — update depth and wait.
		if decoratorParenDepth > 0 {
			decoratorParenDepth += strings.Count(line, "(") - strings.Count(line, ")")
			if decoratorParenDepth < 0 {
				decoratorParenDepth = 0
			}
			continue
		}

		// Any non-decorator, non-def line clears all pending state.
		pendingRoutes = nil
		pendingDecorators = nil
		pendingApiViewMethods = nil

		// Django URL patterns in urls.py files.
		if isURLsFile {
			if m := urlPathRe.FindStringSubmatch(line); m != nil {
				result = append(result, facts.Fact{
					Kind: facts.KindRoute,
					Name: "* " + m[1],
					File: relFile,
					Line: lineNum,
					Props: map[string]any{
						"path":      m[1],
						"handler":   m[2],
						"framework": "django",
						"language":  "python",
					},
				})
			}
		}

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

	if err := scanner.Err(); err != nil {
		log.Printf("[python-extractor] scanner error in %s: %v", relFile, err)
	}

	return result
}

// --- Helpers ---

// applyDecoratorProps sets structural boolean props on a symbol based on a
// decorator name. Only well-known structural decorators produce props; unknown
// decorators are silently ignored.
func applyDecoratorProps(props map[string]any, decoratorName string) {
	// Use the last dot-separated component: "functools.cached_property" → "cached_property".
	last := decoratorName
	if idx := strings.LastIndex(decoratorName, "."); idx >= 0 {
		last = decoratorName[idx+1:]
	}
	switch last {
	case "property", "cached_property":
		props["property"] = true
	case "staticmethod":
		props["static"] = true
	case "classmethod":
		props["class_method"] = true
	case "abstractmethod":
		props["abstract"] = true
	case "task":
		props["task"] = true
	case "shared_task":
		// shared_task is Celery-specific; bare @task is used by Airflow, Prefect, Luigi, etc.
		props["task"] = true
		props["framework"] = "celery"
	}
}

// detectDjango returns true if the project at repoPath uses Django, by scanning
// common dependency files and checking for manage.py.
func detectDjango(repoPath string) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "setup.cfg", "setup.py"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(data)), "django") {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(repoPath, "manage.py"))
	return err == nil
}

// camelToSnake converts a PascalCase class name to the snake_case table name
// Django would auto-generate. e.g. "UserProfile" → "user_profile".
func camelToSnake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			b.WriteByte('_')
		}
		if ch >= 'A' && ch <= 'Z' {
			b.WriteByte(ch + 32) // ASCII lowercase
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// lastComponent returns the last dot-separated segment of a qualified name.
// e.g. "models.Model" → "Model", "Model" → "Model".
func lastComponent(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

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
	if before, _, ok := strings.Cut(s, "["); ok {
		return strings.TrimSpace(before)
	}
	if before, _, ok := strings.Cut(s, "("); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(s)
}

// isPythonFile returns true if the file has a .py extension.
func isPythonFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".py")
}

// readAll reads all bytes from an open file, seeking to the start first.
func readAll(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
