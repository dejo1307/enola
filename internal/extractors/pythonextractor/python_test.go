package pythonextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- Test helpers ---

// writeAndOpen creates a temp file with the given source content, opens it,
// and returns the open *os.File. The file is closed by the caller.
func writeAndOpen(t *testing.T, filename, src string) *os.File {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// byName indexes facts by their Name field for easy lookup.
func byName(ff []facts.Fact) map[string]facts.Fact {
	m := make(map[string]facts.Fact, len(ff))
	for _, f := range ff {
		m[f.Name] = f
	}
	return m
}

// hasRel returns true if fact f has a relation of the given kind to target.
func hasRel(f facts.Fact, kind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

// factsByKind returns all facts with the given Kind.
func factsByKind(ff []facts.Fact, kind string) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// module returns the expected module prefix for a relFile path: strips ".py".
// e.g. "app/models/order.py" → "app/models/order"
func mod(relFile string) string {
	const suffix = ".py"
	if len(relFile) > len(suffix) {
		return relFile[:len(relFile)-len(suffix)]
	}
	return relFile
}

// --- Tests ---

func TestExtractFile_BasicClassAndMethod(t *testing.T) {
	src := `
class Order:
    def __init__(self, total):
        self.total = total

    def calculate(self):
        return self.total * 1.2
`
	f := writeAndOpen(t, "order.py", src)
	defer f.Close()

	relFile := "app/models/order.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	// Class fact: module.ClassName
	clsName := mod(relFile) + ".Order"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing class fact %q; got keys: %v", clsName, keys(idx))
	}
	if cls.Kind != facts.KindSymbol {
		t.Errorf("Order kind = %q, want %q", cls.Kind, facts.KindSymbol)
	}
	if cls.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("Order symbol_kind = %v, want %q", cls.Props["symbol_kind"], facts.SymbolClass)
	}
	if !hasRel(cls, facts.RelDeclares, "app/models") {
		t.Errorf("Order missing declares relation to app/models; relations=%v", cls.Relations)
	}

	// Instance methods: module.ClassName.method
	for _, methodName := range []string{
		mod(relFile) + ".Order.__init__",
		mod(relFile) + ".Order.calculate",
	} {
		m, ok := idx[methodName]
		if !ok {
			t.Fatalf("missing method fact %q", methodName)
		}
		if m.Props["symbol_kind"] != facts.SymbolMethod {
			t.Errorf("%s symbol_kind = %v, want method", methodName, m.Props["symbol_kind"])
		}
	}
}

func TestExtractFile_TopLevelFunction(t *testing.T) {
	src := `
def helper(x, y):
    return x + y

async def fetch_data(url):
    pass
`
	f := writeAndOpen(t, "utils.py", src)
	defer f.Close()

	relFile := "services/utils.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	helperName := mod(relFile) + ".helper"
	helper, ok := idx[helperName]
	if !ok {
		t.Fatalf("missing function fact %q; keys: %v", helperName, keys(idx))
	}
	if helper.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("helper symbol_kind = %v, want function", helper.Props["symbol_kind"])
	}
	if helper.Props["async"] == true {
		t.Error("helper should not have async=true")
	}

	fetchName := mod(relFile) + ".fetch_data"
	fetch, ok := idx[fetchName]
	if !ok {
		t.Fatalf("missing function fact %q", fetchName)
	}
	if fetch.Props["async"] != true {
		t.Errorf("fetch_data async = %v, want true", fetch.Props["async"])
	}
}

func TestExtractFile_ClassInheritance_SingleBase(t *testing.T) {
	src := `
class VespaSink(EmbeddingsSink):
    def send(self, data):
        pass
`
	f := writeAndOpen(t, "vespa_sink.py", src)
	defer f.Close()

	relFile := "sinks/vespa_sink.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	clsName := mod(relFile) + ".VespaSink"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing class fact %q; keys: %v", clsName, keys(idx))
	}
	if !hasRel(cls, facts.RelImplements, "EmbeddingsSink") {
		t.Errorf("VespaSink missing implements relation to EmbeddingsSink; relations = %v", cls.Relations)
	}
}

func TestExtractFile_ClassInheritance_MultipleBases(t *testing.T) {
	src := `
class FeatureGroup(Base, TimestampMixin):
    __tablename__ = "feature_group"

    def validate(self):
        pass
`
	f := writeAndOpen(t, "feature_group.py", src)
	defer f.Close()

	relFile := "db/models/feature_group.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	clsName := mod(relFile) + ".FeatureGroup"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing class fact %q; keys: %v", clsName, keys(idx))
	}
	if !hasRel(cls, facts.RelImplements, "Base") {
		t.Errorf("FeatureGroup missing implements Base; relations = %v", cls.Relations)
	}
	if !hasRel(cls, facts.RelImplements, "TimestampMixin") {
		t.Errorf("FeatureGroup missing implements TimestampMixin; relations = %v", cls.Relations)
	}
}

func TestExtractFile_ClassInheritance_GenericBase(t *testing.T) {
	src := `
class CRUDEntity(CRUDBase[ModelType, IdType]):
    pass
`
	f := writeAndOpen(t, "crud.py", src)
	defer f.Close()

	relFile := "db/crud/crud.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	clsName := mod(relFile) + ".CRUDEntity"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing class fact %q; keys: %v", clsName, keys(idx))
	}
	// Generic parameter should be stripped — base should be "CRUDBase".
	if !hasRel(cls, facts.RelImplements, "CRUDBase") {
		t.Errorf("CRUDEntity missing implements CRUDBase; relations = %v", cls.Relations)
	}
}

func TestExtractFile_NestedClass(t *testing.T) {
	src := `
class Outer:
    class Inner:
        def inner_method(self):
            pass

    def outer_method(self):
        pass
`
	f := writeAndOpen(t, "nested.py", src)
	defer f.Close()

	relFile := "pkg/nested.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	outerName := mod(relFile) + ".Outer"
	if _, ok := idx[outerName]; !ok {
		t.Fatalf("missing %q; keys: %v", outerName, keys(idx))
	}

	// Inner class nested inside Outer.
	innerName := mod(relFile) + ".Outer.Inner"
	if _, ok := idx[innerName]; !ok {
		t.Fatalf("missing %q; keys: %v", innerName, keys(idx))
	}

	// Method of Inner.
	innerMethodName := mod(relFile) + ".Outer.Inner.inner_method"
	innerMethod, ok := idx[innerMethodName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", innerMethodName, keys(idx))
	}
	if innerMethod.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("inner_method symbol_kind = %v, want method", innerMethod.Props["symbol_kind"])
	}

	// Method of Outer (must NOT be prefixed with Inner).
	outerMethodName := mod(relFile) + ".Outer.outer_method"
	outerMethod, ok := idx[outerMethodName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", outerMethodName, keys(idx))
	}
	if outerMethod.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("outer_method symbol_kind = %v, want method", outerMethod.Props["symbol_kind"])
	}
}

func TestExtractFile_AsyncMethod(t *testing.T) {
	src := `
class Recommender:
    async def recommend(self, user_id):
        pass
`
	f := writeAndOpen(t, "recommender.py", src)
	defer f.Close()

	relFile := "services/recommender.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	methodName := mod(relFile) + ".Recommender.recommend"
	m, ok := idx[methodName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", methodName, keys(idx))
	}
	if m.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("recommend symbol_kind = %v, want method", m.Props["symbol_kind"])
	}
	if m.Props["async"] != true {
		t.Errorf("recommend async = %v, want true", m.Props["async"])
	}
}

func TestExtractFile_Imports_BareImport(t *testing.T) {
	src := `
import logging
import os
import fastapi
`
	f := writeAndOpen(t, "app.py", src)
	defer f.Close()

	relFile := "myapp/app.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	for _, target := range []string{"logging", "os", "fastapi"} {
		depName := mod(relFile) + " -> " + target
		dep, ok := idx[depName]
		if !ok {
			t.Errorf("missing dependency fact %q; keys: %v", depName, keys(idx))
			continue
		}
		if dep.Kind != facts.KindDependency {
			t.Errorf("import %q kind = %q, want dependency", target, dep.Kind)
		}
		if !hasRel(dep, facts.RelImports, target) {
			t.Errorf("import %q missing imports relation; relations = %v", target, dep.Relations)
		}
	}
}

func TestExtractFile_Imports_FromImport(t *testing.T) {
	src := `
from fastapi import APIRouter, Depends
from query_recommender.models.filters import VespaSearchFilters
from .base import Base
`
	f := writeAndOpen(t, "routes.py", src)
	defer f.Close()

	relFile := "routes/routes.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	cases := []struct {
		depName string
		target  string
	}{
		{mod(relFile) + " -> fastapi", "fastapi"},
		{mod(relFile) + " -> query_recommender.models.filters", "query_recommender.models.filters"},
		{mod(relFile) + " -> .base", ".base"},
	}

	for _, tc := range cases {
		dep, ok := idx[tc.depName]
		if !ok {
			t.Errorf("missing dependency fact %q; keys: %v", tc.depName, keys(idx))
			continue
		}
		if dep.Props["from"] != true {
			t.Errorf("%q: from prop = %v, want true", tc.depName, dep.Props["from"])
		}
		if !hasRel(dep, facts.RelImports, tc.target) {
			t.Errorf("%q missing imports relation to %q; relations = %v", tc.depName, tc.target, dep.Relations)
		}
	}
}

func TestExtractFile_FastAPIRoute_Get(t *testing.T) {
	src := `
from fastapi import APIRouter

router = APIRouter()

@router.get("/health")
async def health_check():
    return {"status": "ok"}
`
	f := writeAndOpen(t, "health.py", src)
	defer f.Close()

	relFile := "routes/health.py"
	result := extractFile(f, relFile)
	routes := factsByKind(result, facts.KindRoute)

	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d", len(routes))
	}
	r := routes[0]
	if r.Name != "GET /health" {
		t.Errorf("route name = %q, want %q", r.Name, "GET /health")
	}
	if r.Props["http_method"] != "GET" {
		t.Errorf("http_method = %v, want GET", r.Props["http_method"])
	}
	if r.Props["path"] != "/health" {
		t.Errorf("path = %v, want /health", r.Props["path"])
	}
	wantHandler := mod(relFile) + ".health_check"
	if r.Props["handler"] != wantHandler {
		t.Errorf("handler = %v, want %q", r.Props["handler"], wantHandler)
	}
	if r.Props["framework"] != "fastapi" {
		t.Errorf("framework = %v, want fastapi", r.Props["framework"])
	}
}

func TestExtractFile_FastAPIRoute_Post(t *testing.T) {
	src := `
router = APIRouter()

@router.post("/v2/recommend", operation_id="recommend_v2")
async def post_recommend_v2(body: RecommendV2Body) -> RecommendV2Response:
    pass
`
	f := writeAndOpen(t, "recommend.py", src)
	defer f.Close()

	relFile := "routes/recommend.py"
	result := extractFile(f, relFile)
	routes := factsByKind(result, facts.KindRoute)

	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d", len(routes))
	}
	r := routes[0]
	if r.Name != "POST /v2/recommend" {
		t.Errorf("route name = %q, want POST /v2/recommend", r.Name)
	}
	if r.Props["http_method"] != "POST" {
		t.Errorf("http_method = %v, want POST", r.Props["http_method"])
	}
}

func TestExtractFile_FastAPIRoute_MultipleRoutes(t *testing.T) {
	src := `
router = APIRouter()

@router.get("/items")
async def list_items():
    pass

@router.post("/items")
async def create_item():
    pass

@router.delete("/items/{id}")
async def delete_item(id: int):
    pass
`
	f := writeAndOpen(t, "items.py", src)
	defer f.Close()

	result := extractFile(f, "routes/items.py")
	routes := factsByKind(result, facts.KindRoute)

	if len(routes) != 3 {
		t.Fatalf("expected 3 route facts, got %d", len(routes))
	}

	idx := byName(result)
	for _, want := range []string{"GET /items", "POST /items", "DELETE /items/{id}"} {
		if _, ok := idx[want]; !ok {
			t.Errorf("missing route %q", want)
		}
	}
}

func TestExtractFile_FastAPIRoute_MultipleDecorators(t *testing.T) {
	// A handler can have multiple decorators — only the route decorator should
	// produce a route fact; non-route decorators must not clear pending routes.
	src := `
router = APIRouter()

@router.post("/login")
@some_middleware
async def login():
    pass
`
	f := writeAndOpen(t, "auth.py", src)
	defer f.Close()

	result := extractFile(f, "routes/auth.py")
	routes := factsByKind(result, facts.KindRoute)

	if len(routes) != 1 {
		t.Fatalf("expected 1 route fact, got %d: %v", len(routes), routes)
	}
	if routes[0].Name != "POST /login" {
		t.Errorf("route name = %q, want POST /login", routes[0].Name)
	}
}

func TestExtractFile_SQLAlchemyStorage(t *testing.T) {
	src := `
from sqlalchemy.orm import Mapped

class FeatureGroup(Base):
    __tablename__ = "feature_group"

    id: Mapped[int]
    name: Mapped[str]
`
	f := writeAndOpen(t, "feature_group.py", src)
	defer f.Close()

	relFile := "db/models/feature_group.py"
	result := extractFile(f, relFile)
	storages := factsByKind(result, facts.KindStorage)

	if len(storages) != 1 {
		t.Fatalf("expected 1 storage fact, got %d", len(storages))
	}
	s := storages[0]
	if s.Name != "feature_group" {
		t.Errorf("storage name = %q, want feature_group", s.Name)
	}
	if s.Props["storage_kind"] != "table" {
		t.Errorf("storage_kind = %v, want table", s.Props["storage_kind"])
	}
	if s.Props["framework"] != "sqlalchemy" {
		t.Errorf("framework = %v, want sqlalchemy", s.Props["framework"])
	}
	wantClass := mod(relFile) + ".FeatureGroup"
	if s.Props["class"] != wantClass {
		t.Errorf("class = %v, want %q", s.Props["class"], wantClass)
	}
	if !hasRel(s, facts.RelDeclares, "db/models") {
		t.Errorf("storage missing declares relation to db/models; relations = %v", s.Relations)
	}
}

func TestExtractFile_DocstringSkipped(t *testing.T) {
	src := `
class MyService:
    """
    This is a class docstring.
    def fake_def(self):
        pass
    class FakeClass:
        pass
    """

    def real_method(self):
        pass
`
	f := writeAndOpen(t, "service.py", src)
	defer f.Close()

	relFile := "services/service.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	// fake_def and FakeClass inside the docstring must NOT appear.
	fakeDefName := mod(relFile) + ".MyService.fake_def"
	if _, ok := idx[fakeDefName]; ok {
		t.Errorf("fake_def inside docstring should not be extracted")
	}
	fakeClassName := mod(relFile) + ".MyService.FakeClass"
	if _, ok := idx[fakeClassName]; ok {
		t.Errorf("FakeClass inside docstring should not be extracted")
	}

	// real_method must appear.
	realMethodName := mod(relFile) + ".MyService.real_method"
	if _, ok := idx[realMethodName]; !ok {
		t.Errorf("real_method should be extracted; keys: %v", keys(idx))
	}
}

func TestExtractFile_SingleLineDocstringAllowed(t *testing.T) {
	// A one-line triple-quoted string (opens and closes on the same line) must
	// NOT put the extractor into docstring-skip mode.
	src := `
class Validator:
    """One-liner docstring."""

    def validate(self, value):
        pass
`
	f := writeAndOpen(t, "validator.py", src)
	defer f.Close()

	relFile := "pkg/validator.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	validateName := mod(relFile) + ".Validator.validate"
	if _, ok := idx[validateName]; !ok {
		t.Errorf("validate should be extracted after single-line docstring; keys: %v", keys(idx))
	}
}

func TestExtractFile_LineNumbers(t *testing.T) {
	src := `class Foo:
    def bar(self):
        pass
`
	f := writeAndOpen(t, "foo.py", src)
	defer f.Close()

	relFile := "pkg/foo.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	clsName := mod(relFile) + ".Foo"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", clsName, keys(idx))
	}
	if cls.Line != 1 {
		t.Errorf("Foo line = %d, want 1", cls.Line)
	}

	methName := mod(relFile) + ".Foo.bar"
	meth, ok := idx[methName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", methName, keys(idx))
	}
	if meth.Line != 2 {
		t.Errorf("bar line = %d, want 2", meth.Line)
	}
}

func TestExtractFile_ClassWithoutBases(t *testing.T) {
	// A plain `class Foo:` (no parentheses) should produce a class fact with no
	// implements relations.
	src := `
class Foo:
    pass
`
	f := writeAndOpen(t, "foo.py", src)
	defer f.Close()

	relFile := "pkg/foo.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	clsName := mod(relFile) + ".Foo"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", clsName, keys(idx))
	}
	for _, r := range cls.Relations {
		if r.Kind == facts.RelImplements {
			t.Errorf("Foo should have no implements relations, got %v", r)
		}
	}
}

// TestExtractFile_RealWorldFastAPI exercises patterns from svc-example:
// multiple imports, a router variable assignment, and a multi-param async handler.
func TestExtractFile_RealWorldFastAPI(t *testing.T) {
	src := `import logging
from typing import Annotated

from fastapi import APIRouter, Depends

from query_recommender.models.recommend_v2_models import RecommendV2Body

router = APIRouter()

log = logging.getLogger(__name__)


def is_ready() -> bool:
    return True


@router.post("/v2/recommend", operation_id="recommend_v2")
async def post_recommend_v2(
    body: RecommendV2Body,
) -> None:
    pass
`
	f := writeAndOpen(t, "recommend_v2.py", src)
	defer f.Close()

	relFile := "query_recommender/routes/recommend_v2.py"
	result := extractFile(f, relFile)

	routes := factsByKind(result, facts.KindRoute)
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	if routes[0].Name != "POST /v2/recommend" {
		t.Errorf("route name = %q, want POST /v2/recommend", routes[0].Name)
	}

	idx := byName(result)

	// is_ready should be a top-level function.
	isReadyName := mod(relFile) + ".is_ready"
	isReady, ok := idx[isReadyName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", isReadyName, keys(idx))
	}
	if isReady.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("is_ready symbol_kind = %v, want function", isReady.Props["symbol_kind"])
	}

	// post_recommend_v2 is the handler — should also appear as a symbol.
	handlerName := mod(relFile) + ".post_recommend_v2"
	handler, ok := idx[handlerName]
	if !ok {
		t.Fatalf("missing handler symbol %q; keys: %v", handlerName, keys(idx))
	}
	if handler.Props["async"] != true {
		t.Errorf("post_recommend_v2 async = %v, want true", handler.Props["async"])
	}
}

// TestExtractFile_RealWorldSQLAlchemy exercises patterns from feature-store.
func TestExtractFile_RealWorldSQLAlchemy(t *testing.T) {
	src := `from sqlalchemy.orm import Mapped, mapped_column

from .base import Base


class Entity(Base):
    __tablename__ = "entity"

    id: Mapped[int] = mapped_column(primary_key=True)
    name: Mapped[str]

    def __repr__(self) -> str:
        return f"<Entity {self.name}>"
`
	f := writeAndOpen(t, "entity.py", src)
	defer f.Close()

	relFile := "db/models/entity.py"
	result := extractFile(f, relFile)
	idx := byName(result)

	// Class with Base inheritance.
	clsName := mod(relFile) + ".Entity"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", clsName, keys(idx))
	}
	if !hasRel(cls, facts.RelImplements, "Base") {
		t.Errorf("Entity missing implements Base; relations = %v", cls.Relations)
	}

	// Storage fact for the table.
	storages := factsByKind(result, facts.KindStorage)
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage fact, got %d", len(storages))
	}
	if storages[0].Name != "entity" {
		t.Errorf("storage name = %q, want entity", storages[0].Name)
	}

	// __repr__ method.
	reprName := mod(relFile) + ".Entity.__repr__"
	if _, ok := idx[reprName]; !ok {
		t.Errorf("missing __repr__ method; keys: %v", keys(idx))
	}
}

// --- Detect tests ---

func TestDetect_PyprojectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	ok, err := e.Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if !ok {
		t.Error("Detect should return true for pyproject.toml")
	}
}

func TestDetect_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("from setuptools import setup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	ok, err := e.Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if !ok {
		t.Error("Detect should return true for setup.py")
	}
}

func TestDetect_RequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("fastapi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	ok, err := e.Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if !ok {
		t.Error("Detect should return true for requirements.txt")
	}
}

func TestDetect_Pipfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Pipfile"), []byte("[[source]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	ok, err := e.Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if !ok {
		t.Error("Detect should return true for Pipfile")
	}
}

func TestDetect_GoRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	ok, err := e.Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if ok {
		t.Error("Detect should return false for a Go repo without Python markers")
	}
}

// --- Helper unit tests ---

func TestSplitBases_Simple(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"Base", []string{"Base"}},
		{"Base, Mixin", []string{"Base", "Mixin"}},
		{"CRUDBase[Model, Schema]", []string{"CRUDBase"}},
		{"Generic[T], Protocol", []string{"Generic", "Protocol"}},
	}
	for _, tc := range cases {
		got := splitBases(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitBases(%q): got %v (len %d), want %v (len %d)",
				tc.input, got, len(got), tc.want, len(tc.want))
			continue
		}
		for i, g := range got {
			if g != tc.want[i] {
				t.Errorf("splitBases(%q)[%d]: got %q, want %q", tc.input, i, g, tc.want[i])
			}
		}
	}
}

func TestSplitBases_Empty(t *testing.T) {
	got := splitBases("")
	if len(got) != 0 {
		t.Errorf("splitBases(%q): got %v, want empty", "", got)
	}
}

func TestPopScopes(t *testing.T) {
	stack := []scopeEntry{
		{qualifiedName: "pkg.Outer", indent: 0},
		{qualifiedName: "pkg.Outer.Inner", indent: 4},
	}

	// A line at indent=4 should pop Inner (4 >= 4) but keep Outer (0 < 4).
	got := popScopes(stack, 4)
	if len(got) != 1 || got[0].qualifiedName != "pkg.Outer" {
		t.Errorf("popScopes at indent=4: got %v, want [pkg.Outer]", got)
	}

	// A line at indent=0 should pop everything.
	got = popScopes(stack, 0)
	if len(got) != 0 {
		t.Errorf("popScopes at indent=0: got %v, want []", got)
	}
}

func TestLineIndent(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"class Foo:", 0},
		{"    def bar(self):", 4},
		{"        pass", 8},
		{"\t\tpass", 8}, // tab = 4 spaces
		{"", 0},
	}
	for _, tc := range cases {
		got := lineIndent(tc.line)
		if got != tc.want {
			t.Errorf("lineIndent(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

// keys returns map keys sorted for deterministic error messages.
func keys(m map[string]facts.Fact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
