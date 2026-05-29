package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dejo1307/enola/internal/facts"
)

// --- helpers ---

// setupGoProject creates a temp directory with go.mod and Go source files.
// Returns the repo path and a cleanup function.
func setupGoProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	// Create go.mod
	gomod := "module testmod\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create source files
	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func extractAll(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := setupGoProject(t, files)

	// Collect relative file paths
	var relFiles []string
	for f := range files {
		relFiles = append(relFiles, f)
	}

	ext := New()
	result, err := ext.Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return result
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func findFactsByKind(ff []facts.Fact, kind string) []facts.Fact {
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func hasRelation(f facts.Fact, relKind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == relKind && r.Target == target {
			return true
		}
	}
	return false
}

// --- tests ---

func TestExtract_FunctionAndMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/foo.go": `package pkg

func Foo() {}

type Store struct{}

func (s *Store) Bar() {}
`,
	})

	// Check exported function
	foo, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if foo.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("Foo symbol_kind = %v, want function", foo.Props["symbol_kind"])
	}
	if foo.Props["exported"] != true {
		t.Errorf("Foo exported = %v, want true", foo.Props["exported"])
	}

	// Check method with pointer receiver
	bar, ok := findFact(ff, "pkg.Store.Bar")
	if !ok {
		t.Fatal("expected fact for pkg.Store.Bar")
	}
	if bar.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("Bar symbol_kind = %v, want method", bar.Props["symbol_kind"])
	}
	if bar.Props["receiver"] != "Store" {
		t.Errorf("Bar receiver = %v, want Store (star should be stripped)", bar.Props["receiver"])
	}
	if bar.Props["exported"] != true {
		t.Errorf("Bar exported = %v, want true", bar.Props["exported"])
	}
}

func TestExtract_UnexportedSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/internal.go": `package pkg

func helper() {}

type myType struct{}
`,
	})

	fn, ok := findFact(ff, "pkg.helper")
	if !ok {
		t.Fatal("expected fact for pkg.helper")
	}
	if fn.Props["exported"] != false {
		t.Errorf("helper exported = %v, want false", fn.Props["exported"])
	}

	ty, ok := findFact(ff, "pkg.myType")
	if !ok {
		t.Fatal("expected fact for pkg.myType")
	}
	if ty.Props["exported"] != false {
		t.Errorf("myType exported = %v, want false", ty.Props["exported"])
	}
}

func TestExtract_StructWithEmbedding(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/embed.go": `package pkg

import "io"

type Foo struct {
	io.Reader
	Bar
	Name string
}

type Bar struct{}
`,
	})

	foo, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if foo.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("Foo symbol_kind = %v, want struct", foo.Props["symbol_kind"])
	}

	// Should have implements relations for embedded types
	if !hasRelation(foo, facts.RelImplements, "io.Reader") {
		t.Error("Foo should have implements relation for io.Reader")
	}
	if !hasRelation(foo, facts.RelImplements, "Bar") {
		t.Error("Foo should have implements relation for Bar")
	}
}

func TestExtract_InterfaceDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/iface.go": `package pkg

type Fooer interface {
	Foo() error
}
`,
	})

	iface, ok := findFact(ff, "pkg.Fooer")
	if !ok {
		t.Fatal("expected fact for pkg.Fooer")
	}
	if iface.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("Fooer symbol_kind = %v, want interface", iface.Props["symbol_kind"])
	}
	if iface.Props["exported"] != true {
		t.Errorf("Fooer exported = %v, want true", iface.Props["exported"])
	}
}

func TestExtract_GenericType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/generic.go": `package pkg

type Set[T comparable] struct {
	items map[T]struct{}
}
`,
	})

	// The generic type should be named without the type parameter
	set, ok := findFact(ff, "pkg.Set")
	if !ok {
		t.Fatal("expected fact for pkg.Set (without type params)")
	}
	if set.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("Set symbol_kind = %v, want struct", set.Props["symbol_kind"])
	}
}

func TestExtract_CallExtraction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/calls.go": `package pkg

import "fmt"

func DoWork() {
	helper()
	fmt.Println("hello")
}

func helper() {}
`,
	})

	doWork, ok := findFact(ff, "pkg.DoWork")
	if !ok {
		t.Fatal("expected fact for pkg.DoWork")
	}

	// Should have calls relations for helper() and fmt.Println()
	if !hasRelation(doWork, facts.RelCalls, "helper") {
		t.Error("DoWork should have calls relation for helper")
	}
	if !hasRelation(doWork, facts.RelCalls, "fmt.Println") {
		t.Error("DoWork should have calls relation for fmt.Println")
	}
}

func TestExtract_Imports(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/imports.go": `package pkg

import (
	"fmt"
	"io"
)

func Foo() {
	fmt.Println("hello")
	_ = io.EOF
}
`,
	})

	deps := findFactsByKind(ff, facts.KindDependency)
	// Should have imports for "fmt" and "io"
	hasFmt := false
	hasIO := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports && r.Target == "fmt" {
				hasFmt = true
			}
			if r.Kind == facts.RelImports && r.Target == "io" {
				hasIO = true
			}
		}
	}
	if !hasFmt {
		t.Error("expected dependency fact with imports→fmt")
	}
	if !hasIO {
		t.Error("expected dependency fact with imports→io")
	}
}

func TestExtract_PackageGrouping(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/a/a.go": `package a

func FuncA() {}
`,
		"pkg/b/b.go": `package b

func FuncB() {}
`,
	})

	modules := findFactsByKind(ff, facts.KindModule)
	if len(modules) != 2 {
		t.Fatalf("expected 2 module facts, got %d", len(modules))
	}

	modNames := map[string]bool{}
	for _, m := range modules {
		modNames[m.Name] = true
	}
	if !modNames["pkg/a"] {
		t.Error("expected module fact for pkg/a")
	}
	if !modNames["pkg/b"] {
		t.Error("expected module fact for pkg/b")
	}
}

func TestDetect(t *testing.T) {
	ext := New()

	// With go.mod: should detect
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644)
	detected, err := ext.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !detected {
		t.Error("expected Detect=true for directory with go.mod")
	}

	// Without go.mod: should not detect
	dir2 := t.TempDir()
	detected2, err := ext.Detect(dir2)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if detected2 {
		t.Error("expected Detect=false for directory without go.mod")
	}
}
