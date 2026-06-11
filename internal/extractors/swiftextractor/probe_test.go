package swiftextractor

import (
	"os"
	"strings"
	"testing"

	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// TestGrammarSmoke confirms the vendored tree-sitter Swift grammar links and
// parses without panicking. It also fails loudly if the parse tree contains
// ERROR nodes for a trivial valid file (a signal the ABI/grammar is mismatched).
func TestGrammarSmoke(t *testing.T) {
	src := []byte("import Foundation\nstruct Foo { let x: Int }\n")
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(swift.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		t.Fatal("nil root node")
	}
	if root.HasError() {
		t.Fatalf("trivial file parsed with errors:\n%s", root.ToSexp())
	}
}

// TestDumpNodeKinds is a probe used to lock down the exact node-kind strings the
// AST walker depends on. It is skipped by default; run with:
//
//	go test ./internal/extractors/swiftextractor/ -run TestDumpNodeKinds -v -probe
//
// (or temporarily flip the skip) to print the parse tree for the snippet matrix.
func TestDumpNodeKinds(t *testing.T) {
	if os.Getenv("SWIFT_PROBE") == "" {
		t.Skip("probe disabled; set SWIFT_PROBE=1 to run")
	}
	snippets := map[string]string{
		"struct":     "struct Foo: Equatable { let x: Int }",
		"class":      "@MainActor final class Bar: NSObject, ObservableObject { @Published var n = 0 }",
		"enum":       "enum E: String { case a, b }",
		"actor":      "actor A { func work() async {} }",
		"protocol":   "protocol P: AnyObject { func f() }",
		"extension":  "extension Foo: CustomStringConvertible { var description: String { \"\" } }",
		"func":       "func top() { helper(); self.m() }",
		"view":       "struct HomeView: View { @StateObject var vm: HomeViewModel; var body: some View { Text(\"\") } }",
		"inject":     "final class HomeViewModel { init(repo: UserRepository) {} }",
		"injectProp": "final class C { @Injected var repo: UserRepository }",
		"call":       "func go() { let vm = HomeViewModel(); vm.refresh(); AppComposition.shared.makeRepo() }",
		"typealias":  "typealias ID = String",
	}
	parser := sitter.NewParser()
	defer parser.Close()
	_ = parser.SetLanguage(sitter.NewLanguage(swift.Language()))
	for name, src := range snippets {
		tree := parser.Parse([]byte(src), nil)
		t.Logf("\n=== %s ===\nsrc: %s\n%s", name, src, indentSexp(tree.RootNode().ToSexp()))
		tree.Close()
	}
}

// indentSexp lightly pretty-prints a tree-sitter s-expression for readability.
func indentSexp(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			b.WriteByte('\n')
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteRune(r)
			depth++
		case ')':
			depth--
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
