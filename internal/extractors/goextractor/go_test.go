package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
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

	// Bare call resolves to pkgDir.funcName.
	if !hasRelation(doWork, facts.RelCalls, "pkg.helper") {
		t.Error("DoWork should have calls relation for pkg.helper")
	}
	// stdlib alias "fmt" resolves to "fmt" (no module prefix to strip), so target is "fmt.Println".
	if !hasRelation(doWork, facts.RelCalls, "fmt.Println") {
		t.Error("DoWork should have calls relation for fmt.Println")
	}
}

func TestExtract_CallResolution_ImportQualified(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/server.go": `package server

import "testmod/internal/auth"

func Handle() {
	auth.CreateUser()
}
`,
		"internal/auth/auth.go": `package auth

func CreateUser() {}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// "auth" alias maps to "internal/auth", so call resolves to "internal/auth.CreateUser".
	if !hasRelation(handle, facts.RelCalls, "internal/auth.CreateUser") {
		t.Errorf("Handle should call internal/auth.CreateUser; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_ExplicitAlias(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/server.go": `package server

import authlib "testmod/internal/auth"

func Handle() {
	authlib.NewClient()
}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// Explicit alias "authlib" maps to "internal/auth".
	if !hasRelation(handle, facts.RelCalls, "internal/auth.NewClient") {
		t.Errorf("Handle should call internal/auth.NewClient; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_ReceiverMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Handler struct{}

func (h *Handler) DoSomething() {
	h.OtherMethod()
}

func (h *Handler) OtherMethod() {}
`,
	})

	do, ok := findFact(ff, "pkg.Handler.DoSomething")
	if !ok {
		t.Fatal("expected fact for pkg.Handler.DoSomething")
	}
	// h → *Handler receiver; h.OtherMethod() → pkg.Handler.OtherMethod
	if !hasRelation(do, facts.RelCalls, "pkg.Handler.OtherMethod") {
		t.Errorf("DoSomething should call pkg.Handler.OtherMethod; relations: %v", do.Relations)
	}
}

func TestExtract_CallResolution_FieldChain(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/server.go": `package pkg

type Server struct {
	auth AuthService
}

type AuthService struct{}

func (s *Server) Handle() {
	s.auth.Validate()
}

func (a *AuthService) Validate() {}
`,
	})

	handle, ok := findFact(ff, "pkg.Server.Handle")
	if !ok {
		t.Fatal("expected fact for pkg.Server.Handle")
	}
	// s → *Server; s.auth → field type AuthService (same pkg); s.auth.Validate() → pkg.AuthService.Validate
	if !hasRelation(handle, facts.RelCalls, "pkg.AuthService.Validate") {
		t.Errorf("Handle should call pkg.AuthService.Validate; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_Fallback(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/fallback.go": `package pkg

func DoStuff(a interface{}) {
	// a.b.c.d() — unresolvable chain
}
`,
	})

	// Just verify extraction completes without panic (no fact with an unresolvable call target).
	_, ok := findFact(ff, "pkg.DoStuff")
	if !ok {
		t.Fatal("expected fact for pkg.DoStuff")
	}
}

func TestExtract_CallResolution_SelfImport(t *testing.T) {
	// Subpackage imports the module root (exact-match self-import). The resolver
	// must map the import alias to "." so that field-chain resolution produces
	// root-package fact names like "..LocalService.DoWork".
	ff := extractAll(t, map[string]string{
		"auth.go": `package testmod

type AuthLibrary struct {
	Service LocalService
}

type LocalService struct{}
`,
		"adapters/handler.go": `package adapters

import auth "testmod"

type Handler struct {
	authLib *auth.AuthLibrary
}

func (h *Handler) Register() {
	h.authLib.Service.DoWork()
}

func (s *auth.LocalService) DoWork() {}
`,
	})

	register, ok := findFact(ff, "adapters.Handler.Register")
	if !ok {
		t.Fatal("expected fact for adapters.Handler.Register")
	}
	// h.authLib → "..AuthLibrary" (root pkg), .Service → "..LocalService", .DoWork → "..LocalService.DoWork"
	if !hasRelation(register, facts.RelCalls, "..LocalService.DoWork") {
		t.Errorf("Register should call ..LocalService.DoWork; relations: %v", register.Relations)
	}
}

func TestExtract_CallResolution_CrossPackageFieldChain(t *testing.T) {
	// Two packages in the same module. The server package has a struct with a
	// field typed from the auth package; handler method calls through that field.
	ff := extractAll(t, map[string]string{
		"pkg/auth/auth.go": `package auth

type Service struct{}

func (s *Service) ValidateToken() {}
`,
		"pkg/server/server.go": `package server

import "testmod/pkg/auth"

type Handler struct {
	auth auth.Service
}

func (h *Handler) Handle() {
	h.auth.ValidateToken()
}
`,
	})

	handle, ok := findFact(ff, "pkg/server.Handler.Handle")
	if !ok {
		t.Fatal("expected fact for pkg/server.Handler.Handle")
	}
	// h.auth → "pkg/auth.Service", .ValidateToken() → "pkg/auth.Service.ValidateToken"
	if !hasRelation(handle, facts.RelCalls, "pkg/auth.Service.ValidateToken") {
		t.Errorf("Handle should call pkg/auth.Service.ValidateToken; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_LocalConstructor(t *testing.T) {
	// A local variable assigned from a New<Type> constructor in another package;
	// a method call on it should resolve to the canonical method fact name.
	ff := extractAll(t, map[string]string{
		"internal/auth/auth.go": `package auth

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) Validate() {}
`,
		"internal/server/server.go": `package server

import "testmod/internal/auth"

func Handle() {
	svc := auth.NewService()
	svc.Validate()
}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// svc := auth.NewService() infers type internal/auth.Service; svc.Validate()
	// resolves to internal/auth.Service.Validate.
	if !hasRelation(handle, facts.RelCalls, "internal/auth.Service.Validate") {
		t.Errorf("Handle should call internal/auth.Service.Validate; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_LocalCompositeLit(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Service struct{}

func (s *Service) Do() {}

func Run() {
	svc := &Service{}
	svc.Do()
}
`,
	})

	run, ok := findFact(ff, "pkg.Run")
	if !ok {
		t.Fatal("expected fact for pkg.Run")
	}
	if !hasRelation(run, facts.RelCalls, "pkg.Service.Do") {
		t.Errorf("Run should call pkg.Service.Do; relations: %v", run.Relations)
	}
}

func TestExtract_CallResolution_LocalVarDecl(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Service struct{}

func (s *Service) Do() {}

func Run() {
	var svc Service
	svc.Do()
}
`,
	})

	run, ok := findFact(ff, "pkg.Run")
	if !ok {
		t.Fatal("expected fact for pkg.Run")
	}
	if !hasRelation(run, facts.RelCalls, "pkg.Service.Do") {
		t.Errorf("Run should call pkg.Service.Do; relations: %v", run.Relations)
	}
}

func TestExtract_CallResolution_SkipsBuiltins(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/calls.go": `package pkg

func DoWork() {
	_ = len(items())
	_ = make([]int, 0)
	_ = string(raw())
	helper()
}

func items() []int { return nil }
func raw() []byte  { return nil }
func helper()      {}
`,
	})

	doWork, ok := findFact(ff, "pkg.DoWork")
	if !ok {
		t.Fatal("expected fact for pkg.DoWork")
	}
	// Real same-package calls are still emitted.
	if !hasRelation(doWork, facts.RelCalls, "pkg.helper") {
		t.Error("DoWork should call pkg.helper")
	}
	if !hasRelation(doWork, facts.RelCalls, "pkg.items") {
		t.Error("DoWork should call pkg.items")
	}
	// Builtins and conversions must NOT produce dangling edges.
	for _, name := range []string{"pkg.len", "pkg.make", "pkg.string"} {
		if hasRelation(doWork, facts.RelCalls, name) {
			t.Errorf("DoWork should not have a calls edge for builtin %q", name)
		}
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

// TestExtract_StructAndMethodAreSeparateFacts documents the contract the graph's
// has_method synthesis relies on: a struct and each of its methods are emitted as
// distinct sibling facts ("pkg.Type" and "pkg.Type.Method"), with no edge between
// them at extraction time. If this ever changes, internal/facts.NewGraph's
// has_method third pass must be revisited.
func TestExtract_StructAndMethodAreSeparateFacts(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type AuthHandler struct{}

func (h *AuthHandler) Login() {}
`,
	})

	st, ok := findFact(ff, "pkg.AuthHandler")
	if !ok {
		t.Fatal("expected struct fact pkg.AuthHandler")
	}
	if st.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("AuthHandler symbol_kind = %v, want struct", st.Props["symbol_kind"])
	}
	// The struct must NOT carry an edge to its method (the graph synthesizes it).
	for _, rel := range st.Relations {
		if rel.Target == "pkg.AuthHandler.Login" {
			t.Errorf("struct should not declare its method directly, found relation %+v", rel)
		}
	}

	m, ok := findFact(ff, "pkg.AuthHandler.Login")
	if !ok {
		t.Fatal("expected method fact pkg.AuthHandler.Login as a separate fact")
	}
	if m.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("Login symbol_kind = %v, want method", m.Props["symbol_kind"])
	}
}
