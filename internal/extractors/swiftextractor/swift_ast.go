package swiftextractor

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractFileAST parses a Swift file with tree-sitter and emits architectural
// facts. It is a superset of the legacy regex extractor: every declaration /
// import / iOS-classification fact is preserved, and call-graph relations
// (RelCalls / RelInstantiates / RelInjects) plus View->ViewModel RelDependsOn
// edges are attached to symbol facts when call sites, initializer parameters or
// dependency-injection property wrappers are observed.
//
// Edge targets are emitted as bare simple type names here (e.g. "AppComposition")
// and canonicalised to "<dir>.<Type>" by the post-pass in Extract, which has the
// project-wide type index needed to resolve them.
func extractFileAST(src []byte, relFile string, isiOS bool) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(swift.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	w := &astWalker{
		src:     src,
		relFile: relFile,
		dir:     filepath.Dir(relFile),
		isiOS:   isiOS,
	}
	w.walkSourceFile(tree.RootNode())
	return w.out
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string
	isiOS   bool

	// out is the accumulating fact list.
	out []facts.Fact

	// ownerStack holds INDICES into out (not pointers) of the symbol fact whose
	// body is currently being walked. Indices stay valid across append/realloc,
	// which pointers would not. New call/instantiate/inject edges append to
	// out[currentOwner()].Relations. -1 means no owner (top level / extension).
	ownerStack []int

	// typeStack holds the simple names of the enclosing class/struct/.../extension
	// declarations, so members are named "<dir>.<Type>.<member>" (parity with the
	// Go/Kotlin extractors). methodStack is parallel and holds the set of method
	// names declared directly in each enclosing type, used to resolve same-type
	// bare/self calls to "<dir>.<Type>.<method>".
	typeStack   []string
	methodStack []map[string]bool
}

func (w *astWalker) pushType(name string, methods map[string]bool) {
	w.typeStack = append(w.typeStack, name)
	w.methodStack = append(w.methodStack, methods)
}

func (w *astWalker) popType() {
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.methodStack = w.methodStack[:len(w.methodStack)-1]
}

func (w *astWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *astWalker) currentMethods() map[string]bool {
	if len(w.methodStack) == 0 {
		return nil
	}
	return w.methodStack[len(w.methodStack)-1]
}

// qualify prepends the enclosing type path to a declaration's name when inside a
// type ("<Type>.<name>"); at top level it returns name unchanged.
func (w *astWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
}

func (w *astWalker) pushOwner(i int) { w.ownerStack = append(w.ownerStack, i) }
func (w *astWalker) popOwner()        { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }
func (w *astWalker) currentOwner() int {
	if len(w.ownerStack) == 0 {
		return -1
	}
	return w.ownerStack[len(w.ownerStack)-1]
}

// addOwnerEdge appends a relation to the current owner fact, if any, avoiding
// duplicate relations on the same fact.
func (w *astWalker) addOwnerEdge(kind, target string) {
	idx := w.currentOwner()
	if idx < 0 || target == "" {
		return
	}
	for _, r := range w.out[idx].Relations {
		if r.Kind == kind && r.Target == target {
			return
		}
	}
	w.out[idx].Relations = append(w.out[idx].Relations, facts.Relation{Kind: kind, Target: target})
}

func (w *astWalker) walkSourceFile(root *sitter.Node) {
	if root == nil {
		return
	}
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		child := root.Child(i)
		switch child.Kind() {
		case "import_declaration":
			w.handleImport(child)
		case "class_declaration":
			w.handleClassDeclaration(child)
		case "protocol_declaration":
			w.handleProtocol(child)
		case "function_declaration":
			w.handleFunction(child)
		case "property_declaration":
			w.handleProperty(child)
		case "typealias_declaration":
			w.handleTypeAlias(child)
		}
	}
}

func (w *astWalker) handleImport(node *sitter.Node) {
	if m := importRe.FindStringSubmatch(nodeText(node, w.src)); m != nil {
		name := m[1]
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindDependency,
			Name: w.dir + " -> " + name,
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"language": "swift",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: name},
			},
		})
	}
}

// handleClassDeclaration handles class_declaration, which in the Swift grammar
// covers struct / class / enum / actor / extension (distinguished by the leading
// keyword token).
func (w *astWalker) handleClassDeclaration(node *sitter.Node) {
	keyword := declKeyword(node, w.src)
	if keyword == "extension" {
		w.handleExtension(node)
		return
	}

	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}

	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	supertypes := inheritanceNames(node, w.src)

	symbolKind := facts.SymbolClass
	switch keyword {
	case "struct":
		symbolKind = facts.SymbolStruct
	case "enum", "actor", "class":
		symbolKind = facts.SymbolClass
	}

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if keyword == "enum" {
		f.Props["enum"] = true
	}
	if keyword == "actor" {
		f.Props["concurrency"] = "actor"
	}
	if strings.Contains(modifierText, "final") {
		f.Props["final"] = true
	}
	if containsAnnotation(attrs, "MainActor") {
		f.Props["main_actor"] = true
	}
	for _, st := range supertypes {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: st})
	}

	body := typeBody(node)
	if sig, published := computeSignature(body, w.src); sig != "" || len(published) > 0 {
		if sig != "" {
			f.Props["signature"] = sig
		}
		if len(published) > 0 {
			f.Props["reactive"] = true
			f.Props["published_properties"] = strings.Join(published, ",")
		}
	}

	if w.isiOS {
		addIOSProps(&f, name, attrs, strings.Join(supertypes, ", "))
	}
	iosComponent, _ := f.Props["ios_component"].(string)

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1

	w.pushOwner(ownerIdx)
	w.pushType(name, collectMethodNames(body, w.src))
	w.walkTypeBody(body, iosComponent)
	w.popType()
	w.popOwner()
}

func (w *astWalker) handleProtocol(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	supertypes := inheritanceNames(node, w.src)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolInterface,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	for _, st := range supertypes {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: st})
	}
	body := findChildByKind(node, "protocol_body")
	if sig, _ := computeSignature(body, w.src); sig != "" {
		f.Props["signature"] = sig
	}
	if w.isiOS {
		addIOSProps(&f, name, attrs, strings.Join(supertypes, ", "))
	}
	w.out = append(w.out, f)
}

// handleExtension preserves the legacy behaviour: one symbol fact per adopted
// protocol named "<dir>.<Base>+<Proto>". It additionally walks the extension body
// so methods declared in extensions get symbol facts ("<dir>.<Base>.<method>")
// and contribute call-graph edges.
func (w *astWalker) handleExtension(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	base := simpleTypeName(nameNode, w.src)
	if base == "" {
		return
	}
	for _, proto := range inheritanceNames(node, w.src) {
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: w.dir + "." + base + "+" + proto,
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": "extension",
				"exported":    true,
				"language":    "swift",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: w.dir},
				{Kind: facts.RelImplements, Target: proto},
			},
		})
	}

	body := typeBody(node)
	// No single type owner for an extension; methods push their own owner.
	w.pushType(base, collectMethodNames(body, w.src))
	w.walkTypeBody(body, "")
	w.popType()
}

// walkTypeBody iterates the direct members of a type body, emitting member symbol
// facts and attaching call-graph edges. iosComponent is the enclosing type's iOS
// classification (used to detect SwiftUI View->ViewModel dependencies).
func (w *astWalker) walkTypeBody(body *sitter.Node, iosComponent string) {
	if body == nil {
		return
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		switch c.Kind() {
		case "function_declaration":
			w.handleFunction(c)
		case "property_declaration":
			w.handleProperty(c)
			w.handlePropertyInjection(c, iosComponent)
		case "init_declaration":
			w.handleInit(c)
		case "class_declaration":
			w.handleClassDeclaration(c)
		case "protocol_declaration":
			w.handleProtocol(c)
		case "typealias_declaration":
			w.handleTypeAlias(c)
		}
	}
}

func (w *astWalker) handleFunction(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	body := findChildByKind(node, "function_body")
	header := headerText(node, body, w.src)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolFunc,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if t := w.enclosingType(); t != "" {
		f.Props["receiver"] = t
	}
	if strings.Contains(header, " async") {
		f.Props["async"] = true
	}
	if strings.Contains(header, " throws") {
		f.Props["throws"] = true
	}
	if strings.Contains(header, "nonisolated") {
		f.Props["nonisolated"] = true
	}
	if strings.Contains(header, "@MainActor") || containsAnnotation(attrs, "MainActor") {
		f.Props["main_actor"] = true
	}

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1
	w.pushOwner(ownerIdx)
	w.walkForCalls(body)
	w.popOwner()
}

func (w *astWalker) handleProperty(node *sitter.Node) {
	nameNode := propertyNameNode(node, w.src)
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	if name == "" || name == "_" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)

	symbolKind := facts.SymbolVariable
	if vb := findChildByKind(node, "value_binding_pattern"); vb != nil && strings.Contains(nodeText(vb, w.src), "let") {
		symbolKind = facts.SymbolConstant
	}

	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	// A property initializer may construct types (let x = Foo()); attribute those
	// to the enclosing type owner.
	w.walkForCalls(node)
}

// handlePropertyInjection emits DI edges from the enclosing type for a property
// that carries a dependency-injection property wrapper. For SwiftUI Views,
// @StateObject/@ObservedObject/@EnvironmentObject also yield the legacy
// View->ViewModel RelDependsOn edge.
func (w *astWalker) handlePropertyInjection(node *sitter.Node, iosComponent string) {
	modifiers := findChildByKind(node, "modifiers")
	attrs := attributeNames(modifiers, w.src)
	if len(attrs) == 0 {
		return
	}
	ta := findChildByKind(node, "type_annotation")
	if ta == nil {
		return
	}
	typeName := simpleTypeName(firstTypeNode(ta), w.src)
	if typeName == "" || isSystemType(typeName) || !isTypeName(typeName) {
		return
	}

	injectWrappers := []string{"Injected", "Dependency", "Environment", "EnvironmentObject", "ObservedObject", "StateObject"}
	for _, wrapper := range injectWrappers {
		if containsAnnotation(attrs, wrapper) {
			w.addOwnerEdge(facts.RelInjects, typeName)
			break
		}
	}
	if iosComponent == "swiftui_view" {
		for _, wrapper := range []string{"StateObject", "ObservedObject", "EnvironmentObject"} {
			if containsAnnotation(attrs, wrapper) {
				w.addOwnerEdge(facts.RelDependsOn, typeName)
				break
			}
		}
	}
}

// handleInit processes an initializer: each non-system parameter type becomes a
// RelInjects edge on the enclosing type (Swift constructor injection needs no
// annotation), and the body is walked for construction calls.
func (w *astWalker) handleInit(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Kind() != "parameter" {
			continue
		}
		typeName := simpleTypeName(parameterTypeNode(c), w.src)
		if typeName != "" && !isSystemType(typeName) && isTypeName(typeName) {
			w.addOwnerEdge(facts.RelInjects, typeName)
		}
	}
	if body := findChildByKind(node, "function_body"); body != nil {
		w.walkForCalls(body)
	}
}

func (w *astWalker) handleTypeAlias(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolType,
			"exported":    !isPrivateAccess(nodeText(modifiers, w.src)),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
}

// walkForCalls recursively scans an expression subtree for call_expression nodes
// and attaches edges to the current owner:
//   - capitalized callee  -> RelInstantiates (constructor)
//   - bare lowercase call -> RelCalls (resolved via resolveCall)
//   - self.method()       -> RelCalls to the enclosing type's method
//   - Type.member()       -> RelCalls to the receiver type (DI hub / static access)
func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	if node.Kind() == "call_expression" {
		if callee := firstNamedChild(node); callee != nil {
			name, isNav, root := calleeInfo(callee, w.src)
			switch {
			case name == "":
				// unresolved
			case !isNav:
				if isCapitalized(name) {
					if !isSystemType(name) {
						w.addOwnerEdge(facts.RelInstantiates, name)
					}
				} else if target := w.resolveCall(name); target != "" {
					w.addOwnerEdge(facts.RelCalls, target)
				}
			case isNav && (root == "self" || root == "Self"):
				if w.currentMethods()[name] {
					w.addOwnerEdge(facts.RelCalls, w.dir+"."+w.enclosingType()+"."+name)
				}
			case isNav && isCapitalized(root) && !isSystemType(root):
				// e.g. AppComposition.shared.makeRepo() — depend on the root type.
				w.addOwnerEdge(facts.RelCalls, root)
			}
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}
}

// resolveCall maps a bare (non-navigation) call name to a canonical symbol fact
// name: a same-type method, a suppressed stdlib builtin, or a same-module
// top-level function.
func (w *astWalker) resolveCall(name string) string {
	if w.currentMethods()[name] {
		return w.dir + "." + w.enclosingType() + "." + name
	}
	if swiftBuiltins[name] {
		return ""
	}
	return w.dir + "." + name
}

// swiftBuiltins are Swift global / standard-library functions that appear as bare
// calls without an import. Resolving them would create dangling phantom RelCalls
// edges, so they are suppressed (the Swift analog of kotlinBuiltins).
var swiftBuiltins = map[string]bool{
	"print": true, "debugPrint": true, "dump": true,
	"assert": true, "assertionFailure": true, "precondition": true,
	"preconditionFailure": true, "fatalError": true,
	"abs": true, "min": true, "max": true, "swap": true, "zip": true,
	"stride": true, "sequence": true, "repeatElement": true,
	"withUnsafePointer": true, "withExtendedLifetime": true, "withAnimation": true,
	"isKnownUniquelyReferenced": true, "numericCast": true,
}

// --- node helpers ---

// declKeyword returns the declaration keyword token (struct/class/enum/actor/
// extension/protocol) of a class_declaration-like node.
func declKeyword(node *sitter.Node, src []byte) string {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.IsNamed() {
			continue
		}
		switch t := nodeText(c, src); t {
		case "struct", "class", "enum", "actor", "extension", "protocol":
			return t
		}
	}
	return "class"
}

// typeBody returns the class_body or enum_class_body child of a declaration.
func typeBody(node *sitter.Node) *sitter.Node {
	if b := findChildByKind(node, "class_body"); b != nil {
		return b
	}
	return findChildByKind(node, "enum_class_body")
}

// inheritanceNames returns the simple supertype/protocol names from a
// declaration's inheritance_specifier children.
func inheritanceNames(node *sitter.Node, src []byte) []string {
	var names []string
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Kind() != "inheritance_specifier" {
			continue
		}
		tn := c.ChildByFieldName("inherits_from")
		if tn == nil {
			tn = firstNamedChild(c)
		}
		if name := simpleTypeName(tn, src); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// attributeNames returns the simple names of attributes (e.g. ["MainActor",
// "Published"]) declared in a modifiers node.
func attributeNames(modifiers *sitter.Node, src []byte) []string {
	if modifiers == nil {
		return nil
	}
	var out []string
	for i := uint(0); i < uint(modifiers.ChildCount()); i++ {
		c := modifiers.Child(i)
		if c.Kind() != "attribute" {
			continue
		}
		if name := simpleTypeName(firstNamedChild(c), src); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// simpleTypeName extracts the simple (rightmost, generics-stripped) type name
// from a type node by reusing the tested extractTypeName string helper.
func simpleTypeName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return extractTypeName(nodeText(node, src))
}

// firstTypeNode returns the first user_type/type-ish descendant of a node such as
// a type_annotation.
func firstTypeNode(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch c.Kind() {
		case "user_type", "type_identifier", "optional_type", "array_type",
			"dictionary_type", "opaque_type":
			return c
		}
	}
	return firstNamedChild(node)
}

// parameterTypeNode returns the type node of a `parameter` (the second `name`
// field in the Swift grammar is the parameter type).
func parameterTypeNode(param *sitter.Node) *sitter.Node {
	if ta := findChildByKind(param, "type_annotation"); ta != nil {
		return firstTypeNode(ta)
	}
	// Grammar shape: (parameter name: (simple_identifier) name: (user_type ...))
	var last *sitter.Node
	for i := uint(0); i < uint(param.ChildCount()); i++ {
		c := param.Child(i)
		switch c.Kind() {
		case "user_type", "optional_type", "array_type", "dictionary_type", "type_identifier":
			last = c
		}
	}
	return last
}

// propertyNameNode returns the bound identifier node of a property_declaration.
func propertyNameNode(node *sitter.Node, src []byte) *sitter.Node {
	if pat := node.ChildByFieldName("name"); pat != nil {
		if id := pat.ChildByFieldName("bound_identifier"); id != nil {
			return id
		}
		return findFirstIdentifier(pat, src)
	}
	if pat := findChildByKind(node, "pattern"); pat != nil {
		return findFirstIdentifier(pat, src)
	}
	return nil
}

// collectMethodNames returns the set of method names declared directly in a type
// body, used to resolve same-type bare/self calls.
func collectMethodNames(body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if c.Kind() != "function_declaration" {
			continue
		}
		if n := c.ChildByFieldName("name"); n != nil {
			methods[nodeText(n, src)] = true
		}
	}
	return methods
}

// computeSignature reconstructs up to 15 direct member declaration lines from a
// type body (parity with the legacy regex signature capture) and returns the
// names of @Published properties.
func computeSignature(body *sitter.Node, src []byte) (string, []string) {
	if body == nil {
		return "", nil
	}
	const maxMembers = 15
	var members []string
	var published []string
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		kind := c.Kind()
		if kind != "property_declaration" && kind != "function_declaration" {
			continue
		}
		if len(members) < maxMembers {
			members = append(members, memberSignature(c, src))
		}
		if kind == "property_declaration" {
			attrs := attributeNames(findChildByKind(c, "modifiers"), src)
			if containsAnnotation(attrs, "Published") {
				if nn := propertyNameNode(c, src); nn != nil {
					published = append(published, nodeText(nn, src))
				}
			}
		}
	}
	return strings.Join(members, "\n"), published
}

// memberSignature renders a member declaration as a single trimmed line, dropping
// any body/computed block.
func memberSignature(node *sitter.Node, src []byte) string {
	s := nodeText(node, src)
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// headerText returns the source text of node up to the start of its body (or the
// whole node when there is no body), used to inspect a function's signature.
func headerText(node, body *sitter.Node, src []byte) string {
	end := node.EndByte()
	if body != nil {
		end = body.StartByte()
	}
	return string(src[node.StartByte():end])
}

// calleeInfo inspects a call_expression's callee and returns the called simple
// name, whether it was a navigation (a.b.foo()) call, and the leftmost receiver
// identifier ("self" for self-calls; the root type/var name otherwise).
func calleeInfo(callee *sitter.Node, src []byte) (name string, isNav bool, root string) {
	switch callee.Kind() {
	case "simple_identifier", "identifier":
		return nodeText(callee, src), false, ""
	case "navigation_expression":
		suffix := callee.ChildByFieldName("suffix")
		if suffix == nil {
			suffix = findChildByKind(callee, "navigation_suffix")
		}
		if suffix != nil {
			if id := findFirstIdentifier(suffix, src); id != nil {
				name = nodeText(id, src)
			}
		}
		root = navigationRoot(callee, src)
		return name, true, root
	}
	return "", false, ""
}

// navigationRoot drills to the leftmost receiver of a (possibly nested)
// navigation_expression and returns its identifier text ("self" for self).
func navigationRoot(nav *sitter.Node, src []byte) string {
	cur := nav
	for cur != nil && cur.Kind() == "navigation_expression" {
		target := cur.ChildByFieldName("target")
		if target == nil {
			target = firstNamedChild(cur)
		}
		cur = target
	}
	if cur == nil {
		return ""
	}
	if cur.Kind() == "self_expression" {
		return "self"
	}
	return nodeText(cur, src)
}

func isCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}

// isTypeName reports whether s looks like a simple Swift type name (an uppercase
// identifier), filtering out junk like "[String]" before it becomes an edge.
func isTypeName(s string) bool {
	if !isCapitalized(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// --- tree-sitter helpers (mirrors of the Kotlin extractor's) ---

func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); c.Kind() == kind {
			return c
		}
	}
	return nil
}

func firstNamedChild(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); c.IsNamed() {
			return c
		}
	}
	return nil
}

func findFirstIdentifier(node *sitter.Node, src []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	if node.Kind() == "identifier" || node.Kind() == "simple_identifier" || node.Kind() == "type_identifier" {
		return node
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if !c.IsNamed() {
			continue
		}
		if found := findFirstIdentifier(c, src); found != nil {
			return found
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}
