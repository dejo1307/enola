package swiftextractor

import (
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// manifestTarget is a target declared in a Package.swift manifest.
type manifestTarget struct {
	name string
	dir  string // module directory: <pkgDir>/Sources/<name>, or a path: override
	deps []manifestDep
	line int
}

type manifestDep struct {
	name     string // dependency name (target name, or product/package name)
	external bool   // true for .product(...) / cross-package references
}

// parsePackageManifest parses a Swift Package Manager manifest (Package.swift)
// and emits module facts for each declared target plus dependency facts for the
// inter-target dependency graph. It deliberately does NOT run the normal source
// AST walker, so the manifest's `let package = Package(...)` binding and its
// `import PackageDescription` no longer pollute the graph with junk facts.
//
// dirToFile maps a directory to a representative source file discovered in it;
// it is used so each emitted dependency fact's File lives inside the source
// target's directory, which lets the graph's dependency→module bridge
// (see facts.NewGraph) synthesise a module→module "imports" edge.
func parsePackageManifest(src []byte, manifestRel string, dirToFile map[string]string) []facts.Fact {
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

	pkgCall := findPackageCall(tree.RootNode(), src)
	if pkgCall == nil {
		return nil
	}
	args := callValueArguments(pkgCall)
	if args == nil {
		return nil
	}

	pkgDir := filepath.ToSlash(filepath.Dir(manifestRel))
	pkgName := argString(args, "name", src)

	// Collect targets and the set of internal target names → dir.
	var targets []manifestTarget
	internalDirs := make(map[string]string)
	if targetsArr := argValue(args, "targets", src); targetsArr != nil {
		for _, elem := range arrayElements(targetsArr) {
			if elem.Kind() != "call_expression" {
				continue
			}
			switch memberCallName(elem, src) {
			case "target", "executableTarget", "testTarget", "macro", "plugin", "systemLibrary", "binaryTarget":
			default:
				continue
			}
			tArgs := callValueArguments(elem)
			if tArgs == nil {
				continue
			}
			name := argString(tArgs, "name", src)
			if name == "" {
				continue
			}
			dir := pkgDir + "/Sources/" + name
			if p := argString(tArgs, "path", src); p != "" {
				dir = filepath.ToSlash(filepath.Join(pkgDir, p))
			}
			t := manifestTarget{
				name: name,
				dir:  dir,
				line: int(elem.StartPosition().Row) + 1,
			}
			if depsArr := argValue(tArgs, "dependencies", src); depsArr != nil {
				t.deps = parseManifestDeps(depsArr, src)
			}
			targets = append(targets, t)
			internalDirs[name] = dir
		}
	}

	var out []facts.Fact

	// Module fact per declared target (guarantees even an empty/stub target is a
	// node, and tags it with its SPM identity).
	for _, t := range targets {
		out = append(out, facts.Fact{
			Kind: facts.KindModule,
			Name: t.dir,
			File: t.dir,
			Props: map[string]any{
				"language":    "swift",
				"spm_target":  t.name,
				"spm_package": pkgName,
			},
		})
	}

	// Dependency fact per declared edge.
	for _, t := range targets {
		for _, d := range t.deps {
			targetDir := d.name
			source := "external"
			if dir, ok := internalDirs[d.name]; ok && !d.external {
				targetDir = dir
				source = "internal"
			}
			out = append(out, facts.Fact{
				Kind: facts.KindDependency,
				Name: t.dir + " -> " + targetDir,
				File: representativeFile(t.dir, dirToFile),
				Line: t.line,
				Props: map[string]any{
					"language": "swift",
					"source":   source,
					"spm":      true,
					"manifest": manifestRel,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: targetDir},
				},
			})
		}
	}

	return out
}

// parseManifestDeps reads a target's `dependencies:` array. Each element is a
// bare string ("Foo"), a `.product(name:package:)` (external), or a
// `.target(name:)`/`.byName(name:)` (internal-by-name).
func parseManifestDeps(arr *sitter.Node, src []byte) []manifestDep {
	var deps []manifestDep
	for _, elem := range arrayElements(arr) {
		switch elem.Kind() {
		case "line_string_literal":
			if name := stringLiteralText(elem, src); name != "" {
				deps = append(deps, manifestDep{name: name})
			}
		case "call_expression":
			member := memberCallName(elem, src)
			cArgs := callValueArguments(elem)
			name := argString(cArgs, "name", src)
			if name == "" {
				continue
			}
			deps = append(deps, manifestDep{name: name, external: member == "product"})
		}
	}
	return deps
}

// representativeFile returns a file path inside dir so that the graph's
// dependency→module bridge (fileDirectory(File) == dir) fires. It prefers a real
// source file discovered in the walk; otherwise it falls back to a synthetic
// Package.swift path inside the directory (used only for module derivation).
func representativeFile(dir string, dirToFile map[string]string) string {
	if f, ok := dirToFile[dir]; ok {
		return f
	}
	return dir + "/Package.swift"
}

// --- manifest tree-sitter helpers ---

// findPackageCall returns the first `Package(...)` call_expression in the tree.
func findPackageCall(node *sitter.Node, src []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	if node.Kind() == "call_expression" {
		if callee := firstNamedChild(node); callee != nil &&
			(callee.Kind() == "simple_identifier" || callee.Kind() == "identifier") &&
			nodeText(callee, src) == "Package" {
			return node
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if found := findPackageCall(node.Child(i), src); found != nil {
			return found
		}
	}
	return nil
}

// callValueArguments returns the value_arguments node of a call_expression.
func callValueArguments(call *sitter.Node) *sitter.Node {
	if call == nil {
		return nil
	}
	if suffix := findChildByKind(call, "call_suffix"); suffix != nil {
		return findChildByKind(suffix, "value_arguments")
	}
	return findChildByKind(call, "value_arguments")
}

// argValue returns the value node of the labeled argument `label` within a
// value_arguments node, or nil.
func argValue(valueArgs *sitter.Node, label string, src []byte) *sitter.Node {
	if valueArgs == nil {
		return nil
	}
	for i := uint(0); i < uint(valueArgs.ChildCount()); i++ {
		arg := valueArgs.Child(i)
		if arg.Kind() != "value_argument" {
			continue
		}
		nameNode := arg.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		if argLabelText(nameNode, src) == label {
			if v := arg.ChildByFieldName("value"); v != nil {
				return v
			}
			// Fall back: the value is the last named child after the label.
			return lastNamedChildExcept(arg, nameNode)
		}
	}
	return nil
}

// argString returns the string value of labeled argument `label`, or "".
func argString(valueArgs *sitter.Node, label string, src []byte) string {
	return stringLiteralText(argValue(valueArgs, label, src), src)
}

// argLabelText returns the identifier text of a value_argument_label node.
func argLabelText(nameNode *sitter.Node, src []byte) string {
	if nameNode == nil {
		return ""
	}
	if id := findFirstIdentifier(nameNode, src); id != nil {
		return nodeText(id, src)
	}
	return strings.TrimSpace(nodeText(nameNode, src))
}

// memberCallName returns the called member/function name of a call_expression:
// the trailing identifier of an implicit-member callee (`.target` -> "target"),
// or a bare identifier callee (`Package` -> "Package").
func memberCallName(call *sitter.Node, src []byte) string {
	callee := firstNamedChild(call)
	if callee == nil {
		return ""
	}
	switch callee.Kind() {
	case "simple_identifier", "identifier":
		return nodeText(callee, src)
	case "prefix_expression":
		if t := callee.ChildByFieldName("target"); t != nil {
			return nodeText(t, src)
		}
		if id := findFirstIdentifier(callee, src); id != nil {
			return nodeText(id, src)
		}
	}
	return ""
}

// stringLiteralText returns the unquoted text of a line_string_literal node (its
// `text` field), or "" for nil / empty / non-string nodes.
func stringLiteralText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	if node.Kind() == "line_string_literal" {
		if t := node.ChildByFieldName("text"); t != nil {
			return nodeText(t, src)
		}
		// Empty string literal "" has no text child.
		return strings.Trim(nodeText(node, src), `"`)
	}
	return ""
}

// arrayElements returns the element expression nodes of an array_literal.
func arrayElements(arr *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	if arr == nil || arr.Kind() != "array_literal" {
		return out
	}
	for i := uint(0); i < uint(arr.ChildCount()); i++ {
		c := arr.Child(i)
		if c.IsNamed() {
			out = append(out, c)
		}
	}
	return out
}

// lastNamedChildExcept returns the last named child of node that is not `except`
// (compared by source span).
func lastNamedChildExcept(node, except *sitter.Node) *sitter.Node {
	var last *sitter.Node
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.IsNamed() && !(c.StartByte() == except.StartByte() && c.EndByte() == except.EndByte()) {
			last = c
		}
	}
	return last
}
