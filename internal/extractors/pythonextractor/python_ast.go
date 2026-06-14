package pythonextractor

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractFileAST parses a Python file with tree-sitter and emits architectural
// facts. It is a superset of extractFile: every symbol / import / route / storage
// fact is preserved, and RelCalls / RelInstantiates edges are added when call
// sites are observed inside function bodies.
func extractFileAST(src []byte, relFile string, isDjango bool) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		return nil
	}

	tree := parser.Parse(src, nil)
	defer tree.Close()

	module := strings.TrimSuffix(relFile, ".py")
	dir := filepath.Dir(relFile)

	w := &pyWalker{
		src:      src,
		relFile:  relFile,
		module:   module,
		dir:      dir,
		isDjango: isDjango,
	}
	w.walkModule(tree.RootNode())
	return w.out
}

type pyWalker struct {
	src      []byte
	relFile  string
	module   string
	dir      string
	isDjango bool

	out []facts.Fact

	// typeStack holds enclosing class names so methods get qualified names.
	typeStack []string

	// ownerStack: top element is the fact that receives RelCalls / RelInstantiates
	// discovered while walking its body.
	ownerStack []*facts.Fact

	// importMap maps a local name to its canonical fact target (empty = external).
	importMap map[string]string

	// methodSets[i] is the set of methods declared directly in typeStack[i],
	// used to resolve bare same-class calls.
	methodSets []map[string]bool
}

func (w *pyWalker) pushOwner(f *facts.Fact) { w.ownerStack = append(w.ownerStack, f) }
func (w *pyWalker) popOwner()               { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }
func (w *pyWalker) currentOwner() *facts.Fact {
	if len(w.ownerStack) == 0 {
		return nil
	}
	return w.ownerStack[len(w.ownerStack)-1]
}

func (w *pyWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *pyWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
}

func (w *pyWalker) pushType(name string, methods map[string]bool) {
	w.typeStack = append(w.typeStack, name)
	w.methodSets = append(w.methodSets, methods)
}

func (w *pyWalker) popType() {
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.methodSets = w.methodSets[:len(w.methodSets)-1]
}

func (w *pyWalker) currentMethods() map[string]bool {
	if len(w.methodSets) == 0 {
		return nil
	}
	return w.methodSets[len(w.methodSets)-1]
}

// walkModule iterates the top-level statements of a module node.
func (w *pyWalker) walkModule(root *sitter.Node) {
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		w.walkStatement(root.Child(i))
	}
}

func (w *pyWalker) walkStatement(node *sitter.Node) {
	if node == nil {
		return
	}
	switch node.Kind() {
	case "import_statement":
		w.handleImport(node)
	case "import_from_statement":
		w.handleFromImport(node)
	case "class_definition":
		w.handleClass(node, nil)
	case "function_definition":
		w.handleFunction(node, nil)
	case "decorated_definition":
		w.handleDecoratedDefinition(node)
	case "expression_statement":
		// __tablename__ = "foo" (SQLAlchemy) lives here at class body level.
		w.handleExprStatement(node)
	case "block":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkStatement(node.Child(i))
		}
	}
}

// handleImport handles `import foo.bar` — emits KindDependency + RelImports.
func (w *pyWalker) handleImport(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Kind() == "dotted_name" || c.Kind() == "aliased_import" {
			var name, alias string
			if c.Kind() == "aliased_import" {
				nameNode := c.ChildByFieldName("name")
				aliasNode := c.ChildByFieldName("alias")
				if nameNode == nil {
					continue
				}
				name = pyText(c.ChildByFieldName("name"), w.src)
				if aliasNode != nil {
					alias = pyText(aliasNode, w.src)
				}
			} else {
				name = pyText(c, w.src)
			}
			target := w.dir + " -> " + name
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindDependency,
				Name: target,
				File: w.relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{"language": "python"},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: name},
				},
			})
			local := alias
			if local == "" {
				if dot := strings.LastIndex(name, "."); dot >= 0 {
					local = name[dot+1:]
				} else {
					local = name
				}
			}
			w.setImport(local, "")
		}
	}
}

// handleFromImport handles `from foo.bar import Baz, Qux`.
func (w *pyWalker) handleFromImport(node *sitter.Node) {
	moduleNode := node.ChildByFieldName("module_name")
	if moduleNode == nil {
		return
	}
	moduleName := pyText(moduleNode, w.src)

	// Determine if this is an intra-project import (relative or same-tree dotted).
	isRelative := strings.HasPrefix(moduleName, ".") ||
		strings.HasPrefix(pyText(node, w.src), "from .")

	target := w.dir + " -> " + moduleName
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindDependency,
		Name: target,
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{"language": "python"},
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: moduleName},
		},
	})

	// Map each imported name to a resolvable target or "" (external).
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Kind() != "dotted_name" && c.Kind() != "identifier" && c.Kind() != "aliased_import" {
			continue
		}
		var localName, importedName string
		if c.Kind() == "aliased_import" {
			n := c.ChildByFieldName("name")
			a := c.ChildByFieldName("alias")
			if n == nil {
				continue
			}
			importedName = pyText(n, w.src)
			if a != nil {
				localName = pyText(a, w.src)
			} else {
				localName = importedName
			}
		} else {
			importedName = pyText(c, w.src)
			localName = importedName
		}

		if isRelative {
			// Relative import → resolve to a local module path.
			base := moduleName
			if strings.HasPrefix(base, ".") {
				base = w.dir + "/" + strings.TrimLeft(base, ".")
			}
			w.setImport(localName, base+"."+importedName)
		} else {
			// External or ambiguous — suppress call edges to this name.
			w.setImport(localName, "")
		}
	}
}

func (w *pyWalker) setImport(local, target string) {
	if local == "" || local == "*" {
		return
	}
	if w.importMap == nil {
		w.importMap = make(map[string]string)
	}
	w.importMap[local] = target
}

// handleDecoratedDefinition unwraps `@decorator\ndef/class ...` nodes.
func (w *pyWalker) handleDecoratedDefinition(node *sitter.Node) {
	var decorators []string
	var pendingApiViewMethods []string

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch c.Kind() {
		case "decorator":
			text := pyText(c, w.src)
			// FastAPI / Starlette route decorator.
			if m := routeDecoratorRe.FindStringSubmatch(text); m != nil {
				w.out = append(w.out, facts.Fact{
					Kind: facts.KindRoute,
					Name: strings.ToUpper(m[2]) + " " + m[3],
					File: w.relFile,
					Line: int(c.StartPosition().Row) + 1,
					Props: map[string]any{
						"method":    strings.ToUpper(m[2]),
						"path":      m[3],
						"framework": "fastapi",
					},
				})
				continue
			}
			// DRF @api_view(['GET','POST']).
			if m := apiViewRe.FindStringSubmatch(text); m != nil {
				for _, meth := range httpMethodWordRe.FindAllString(m[1], -1) {
					pendingApiViewMethods = append(pendingApiViewMethods, meth)
				}
				continue
			}
			// Generic decorator name capture.
			if m := decoratorRe.FindStringSubmatch(text); m != nil {
				decorators = append(decorators, m[1])
			}

		case "function_definition":
			w.handleFunction(c, decorators)
			// @api_view routes — emit after we know the handler name.
			if len(pendingApiViewMethods) > 0 {
				handlerName := w.module + "." + w.qualify(pyFuncName(c, w.src))
				for _, meth := range pendingApiViewMethods {
					w.out = append(w.out, facts.Fact{
						Kind: facts.KindRoute,
						Name: meth + " (view) " + handlerName,
						File: w.relFile,
						Line: int(c.StartPosition().Row) + 1,
						Props: map[string]any{
							"method":    meth,
							"framework": "django",
							"handler":   handlerName,
						},
					})
				}
			}

		case "class_definition":
			w.handleClass(c, decorators)
		}
	}
}

// handleClass emits a KindSymbol fact for a class and walks its body.
func (w *pyWalker) handleClass(node *sitter.Node, decorators []string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := pyText(nameNode, w.src)
	qualName := w.module + "." + w.qualify(name)

	props := map[string]any{
		"symbol_kind": facts.SymbolClass,
		"language":    "python",
	}
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}

	// Superclasses.
	var bases []string
	if args := node.ChildByFieldName("superclasses"); args != nil {
		for i := uint(0); i < uint(args.ChildCount()); i++ {
			c := args.Child(i)
			if c.Kind() == "identifier" || c.Kind() == "attribute" {
				base := pyText(c, w.src)
				bases = append(bases, base)
				rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: base})
			}
		}
	}

	for _, dec := range decorators {
		applyDecoratorProps(props, dec)
	}

	// Django classification.
	if w.isDjango {
		for _, base := range bases {
			last := lastComponent(base)
			if djangoModelBases[last] {
				props["framework"] = "django"
				tableName := camelToSnake(name)
				w.out = append(w.out, facts.Fact{
					Kind: facts.KindStorage,
					Name: tableName,
					File: w.relFile,
					Line: int(node.StartPosition().Row) + 1,
					Props: map[string]any{
						"storage_kind": "table",
						"framework":    "django",
						"class":        qualName,
					},
				})
				break
			}
			if djangoCBVBases[last] {
				props["django_component"] = "view"
				props["framework"] = "django"
				break
			}
			if djangoSerializerBases[last] {
				props["django_component"] = "serializer"
				props["framework"] = "django"
				break
			}
		}
	}

	// Django urls.py: emit route facts from path()/re_path() calls in the class body.
	if w.isDjango && filepath.Base(w.relFile) == "urls.py" {
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil {
			bodyText := pyText(bodyNode, w.src)
			for _, m := range urlPathRe.FindAllStringSubmatch(bodyText, -1) {
				w.out = append(w.out, facts.Fact{
					Kind: facts.KindRoute,
					Name: "* " + m[1],
					File: w.relFile,
					Line: int(node.StartPosition().Row) + 1,
					Props: map[string]any{
						"path":      m[1],
						"handler":   m[2],
						"framework": "django",
					},
				})
			}
		}
	}

	f := facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qualName,
		File:      w.relFile,
		Line:      int(node.StartPosition().Row) + 1,
		Props:     props,
		Relations: rels,
	}

	w.out = append(w.out, f)
	owner := &w.out[len(w.out)-1]
	w.pushOwner(owner)

	bodyNode := node.ChildByFieldName("body")
	w.pushType(name, collectPyMethodNames(bodyNode, w.src))
	if bodyNode != nil {
		w.walkBody(bodyNode)
	}
	w.popType()
	w.popOwner()
}

// handleFunction emits a KindSymbol fact for a function/method.
func (w *pyWalker) handleFunction(node *sitter.Node, decorators []string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := pyText(nameNode, w.src)
	qualName := w.module + "." + w.qualify(name)

	props := map[string]any{
		"symbol_kind": facts.SymbolFunc,
		"language":    "python",
	}
	if len(w.typeStack) > 0 {
		props["receiver"] = w.typeStack[len(w.typeStack)-1]
	}

	// async keyword: look for it as a sibling before the `def` keyword.
	fullText := pyText(node, w.src)
	if strings.HasPrefix(strings.TrimSpace(fullText), "async ") {
		props["async"] = true
	}

	// Return type.
	if retNode := node.ChildByFieldName("return_type"); retNode != nil {
		rt := strings.TrimSpace(pyText(retNode, w.src))
		if strings.HasPrefix(rt, "->") {
			rt = strings.TrimSpace(rt[2:])
		}
		if rt != "" {
			props["return_type"] = rt
		}
	}

	for _, dec := range decorators {
		applyDecoratorProps(props, dec)
	}

	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}

	f := facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qualName,
		File:      w.relFile,
		Line:      int(node.StartPosition().Row) + 1,
		Props:     props,
		Relations: rels,
	}

	w.out = append(w.out, f)
	owner := &w.out[len(w.out)-1]
	w.pushOwner(owner)
	if bodyNode := node.ChildByFieldName("body"); bodyNode != nil {
		w.walkForCalls(bodyNode)
	}
	w.popOwner()
}

// handleExprStatement checks for SQLAlchemy __tablename__ assignments.
func (w *pyWalker) handleExprStatement(node *sitter.Node) {
	text := pyText(node, w.src)
	if m := tableNameRe.FindStringSubmatch(text); m != nil {
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindStorage,
			Name: m[1],
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"storage_kind": "table",
				"framework":    "sqlalchemy",
			},
		})
	}
}

// walkBody walks a class body, dispatching each statement.
func (w *pyWalker) walkBody(body *sitter.Node) {
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		w.walkStatement(body.Child(i))
	}
}

// walkForCalls recursively scans a function body for call nodes and emits
// RelCalls / RelInstantiates on the current owner.
func (w *pyWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	if node.Kind() == "call" {
		if fn := node.ChildByFieldName("function"); fn != nil {
			w.emitCallEdge(fn)
		}
	}
	// Don't recurse into nested class/function definitions — they get their own owner.
	switch node.Kind() {
	case "class_definition", "function_definition", "decorated_definition":
		return
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}
}

// emitCallEdge resolves the callee node and appends a relation to the current owner.
func (w *pyWalker) emitCallEdge(fn *sitter.Node) {
	owner := w.currentOwner()
	if owner == nil {
		return
	}

	switch fn.Kind() {
	case "identifier":
		name := pyText(fn, w.src)
		if pyBuiltins[name] {
			return
		}
		if pyCapitalized(name) {
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelInstantiates,
				Target: name,
			})
			return
		}
		if target := w.resolveCall(name); target != "" {
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: target,
			})
		}

	case "attribute":
		// self.method() or obj.method() — only resolve self.method.
		objNode := fn.ChildByFieldName("object")
		attrNode := fn.ChildByFieldName("attribute")
		if objNode == nil || attrNode == nil {
			return
		}
		obj := pyText(objNode, w.src)
		attr := pyText(attrNode, w.src)
		if obj == "self" || obj == "cls" {
			if methods := w.currentMethods(); methods[attr] {
				owner.Relations = append(owner.Relations, facts.Relation{
					Kind:   facts.RelCalls,
					Target: w.module + "." + w.enclosingType() + "." + attr,
				})
			}
		}
	}
}

// resolveCall maps a bare call name to a canonical fact target.
func (w *pyWalker) resolveCall(name string) string {
	// Same-class method.
	if methods := w.currentMethods(); methods[name] {
		return w.module + "." + w.enclosingType() + "." + name
	}
	// Imported name.
	if target, ok := w.importMap[name]; ok {
		return target // "" means external → no edge
	}
	// Same-module top-level function.
	return w.module + "." + name
}

// collectPyMethodNames returns the set of function names declared directly in a
// class body node.
func collectPyMethodNames(body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		var fn *sitter.Node
		switch c.Kind() {
		case "function_definition":
			fn = c
		case "decorated_definition":
			for j := uint(0); j < uint(c.ChildCount()); j++ {
				if c.Child(j).Kind() == "function_definition" {
					fn = c.Child(j)
					break
				}
			}
		}
		if fn != nil {
			if nameNode := fn.ChildByFieldName("name"); nameNode != nil {
				methods[pyText(nameNode, src)] = true
			}
		}
	}
	return methods
}

func pyFuncName(node *sitter.Node, src []byte) string {
	if n := node.ChildByFieldName("name"); n != nil {
		return pyText(n, src)
	}
	return ""
}

func pyText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}

func pyCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}

// pyBuiltins are Python built-in functions that appear as bare calls without
// an import and have no local fact — resolving them would produce phantom edges.
var pyBuiltins = map[string]bool{
	"print": true, "len": true, "range": true, "enumerate": true, "zip": true,
	"map": true, "filter": true, "sorted": true, "reversed": true, "list": true,
	"dict": true, "set": true, "tuple": true, "str": true, "int": true,
	"float": true, "bool": true, "bytes": true, "type": true, "isinstance": true,
	"issubclass": true, "hasattr": true, "getattr": true, "setattr": true,
	"delattr": true, "callable": true, "repr": true, "hash": true, "id": true,
	"abs": true, "round": true, "min": true, "max": true, "sum": true,
	"any": true, "all": true, "next": true, "iter": true, "open": true,
	"super": true, "object": true, "property": true, "staticmethod": true,
	"classmethod": true, "vars": true, "dir": true, "globals": true,
	"locals": true, "exec": true, "eval": true, "compile": true,
	"input": true, "format": true, "chr": true, "ord": true, "hex": true,
	"oct": true, "bin": true, "pow": true, "divmod": true, "slice": true,
	"NotImplemented": true, "Exception": true, "ValueError": true,
	"TypeError": true, "KeyError": true, "IndexError": true,
	"AttributeError": true, "RuntimeError": true, "StopIteration": true,
	"GeneratorExit": true, "SystemExit": true, "KeyboardInterrupt": true,
}
