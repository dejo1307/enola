package pythonextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- Test helpers ---

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
	relFile := "app/models/order.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "services/utils.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "sinks/vespa_sink.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "db/models/feature_group.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "db/crud/crud.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "pkg/nested.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "services/recommender.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "myapp/app.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "routes/routes.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "routes/health.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "routes/recommend.py"
	result := astExtract(t, relFile, src, false)
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
	result := astExtract(t, "routes/items.py", src, false)
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
	result := astExtract(t, "routes/auth.py", src, false)
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
	relFile := "db/models/feature_group.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "services/service.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "pkg/validator.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "pkg/foo.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "pkg/foo.py"
	result := astExtract(t, relFile, src, false)
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
	relFile := "query_recommender/routes/recommend_v2.py"
	result := astExtract(t, relFile, src, false)

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
	relFile := "db/models/entity.py"
	result := astExtract(t, relFile, src, false)
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


// keys returns map keys sorted for deterministic error messages.
func keys(m map[string]facts.Fact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- Phase 1a: Decorator tracking ---

func TestExtractFile_DecoratorProps_Property(t *testing.T) {
	src := `
class Config:
    @property
    def name(self) -> str:
        return self._name

    @cached_property
    def items(self):
        return []
`
	relFile := "app/config.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	for _, methodName := range []string{
		mod(relFile) + ".Config.name",
		mod(relFile) + ".Config.items",
	} {
		m, ok := idx[methodName]
		if !ok {
			t.Fatalf("missing %q; keys: %v", methodName, keys(idx))
		}
		if m.Props["property"] != true {
			t.Errorf("%s: property = %v, want true", methodName, m.Props["property"])
		}
	}
}

func TestExtractFile_DecoratorProps_Staticmethod(t *testing.T) {
	src := `
class Utils:
    @staticmethod
    def parse(value):
        return int(value)
`
	relFile := "pkg/utils.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	methName := mod(relFile) + ".Utils.parse"
	m, ok := idx[methName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", methName, keys(idx))
	}
	if m.Props["static"] != true {
		t.Errorf("parse: static = %v, want true", m.Props["static"])
	}
	if m.Props["class_method"] == true {
		t.Error("parse: class_method should not be set")
	}
}

func TestExtractFile_DecoratorProps_Classmethod(t *testing.T) {
	src := `
class Repo:
    @classmethod
    def from_env(cls):
        pass
`
	relFile := "db/repo.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	methName := mod(relFile) + ".Repo.from_env"
	m, ok := idx[methName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", methName, keys(idx))
	}
	if m.Props["class_method"] != true {
		t.Errorf("from_env: class_method = %v, want true", m.Props["class_method"])
	}
	if m.Props["static"] == true {
		t.Error("from_env: static should not be set")
	}
}

func TestExtractFile_DecoratorProps_Abstractmethod(t *testing.T) {
	src := `
from abc import ABC, abstractmethod

class Base(ABC):
    @abstractmethod
    def execute(self):
        pass

    def concrete(self):
        pass
`
	relFile := "core/base.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	executeName := mod(relFile) + ".Base.execute"
	execute, ok := idx[executeName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", executeName, keys(idx))
	}
	if execute.Props["abstract"] != true {
		t.Errorf("execute: abstract = %v, want true", execute.Props["abstract"])
	}

	// concrete() must NOT have abstract set.
	concreteName := mod(relFile) + ".Base.concrete"
	concrete, ok := idx[concreteName]
	if !ok {
		t.Fatalf("missing %q", concreteName)
	}
	if concrete.Props["abstract"] == true {
		t.Error("concrete: abstract should not be set")
	}
}

func TestExtractFile_DecoratorProps_StackedDecorators(t *testing.T) {
	// @classmethod + @abstractmethod on the same method — both props must be set.
	src := `
from abc import abstractmethod

class Base:
    @classmethod
    @abstractmethod
    def create(cls):
        pass
`
	relFile := "core/base.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	methName := mod(relFile) + ".Base.create"
	m, ok := idx[methName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", methName, keys(idx))
	}
	if m.Props["class_method"] != true {
		t.Errorf("create: class_method = %v, want true", m.Props["class_method"])
	}
	if m.Props["abstract"] != true {
		t.Errorf("create: abstract = %v, want true", m.Props["abstract"])
	}
}

func TestExtractFile_Task_Bare(t *testing.T) {
	// Bare @task — framework-agnostic (Airflow, Prefect, etc.).
	src := `
@task
def process_records():
    pass
`
	relFile := "jobs/tasks.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	fnName := mod(relFile) + ".process_records"
	fn, ok := idx[fnName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", fnName, keys(idx))
	}
	if fn.Props["task"] != true {
		t.Errorf("process_records: task = %v, want true", fn.Props["task"])
	}
	if fn.Props["framework"] != nil {
		t.Errorf("process_records: framework = %v, want nil for bare @task", fn.Props["framework"])
	}
}

func TestExtractFile_Task_SharedTask(t *testing.T) {
	// @shared_task is Celery-specific and must set framework="celery".
	src := `
from celery import shared_task

@shared_task
def send_welcome_email(user_id: int) -> None:
    pass
`
	relFile := "notifications/email_tasks.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	fnName := mod(relFile) + ".send_welcome_email"
	fn, ok := idx[fnName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", fnName, keys(idx))
	}
	if fn.Props["task"] != true {
		t.Errorf("send_welcome_email: task = %v, want true", fn.Props["task"])
	}
	if fn.Props["framework"] != "celery" {
		t.Errorf("send_welcome_email: framework = %v, want celery", fn.Props["framework"])
	}
}

func TestExtractFile_MultiLineDecorator(t *testing.T) {
	// Multi-line decorator args must not clear pending state before the def.
	// Without the bracket-depth fix the continuation lines clear pendingDecorators.
	src := `
@task(
    bind=True,
    max_retries=3,
)
def retry_task(self):
    pass
`
	relFile := "jobs/tasks.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	fnName := mod(relFile) + ".retry_task"
	fn, ok := idx[fnName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", fnName, keys(idx))
	}
	if fn.Props["task"] != true {
		t.Errorf("retry_task: task = %v, want true (multi-line decorator must survive)", fn.Props["task"])
	}
}

// --- Phase 1b: Return type hints ---

func TestExtractFile_ReturnType_SingleLine(t *testing.T) {
	src := `
def is_ready() -> bool:
    return True

def get_count() -> int:
    return 42

def no_annotation():
    pass
`
	relFile := "pkg/funcs.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	cases := []struct {
		name string
		want string
	}{
		{mod(relFile) + ".is_ready", "bool"},
		{mod(relFile) + ".get_count", "int"},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q", tc.name)
		}
		if fn.Props["return_type"] != tc.want {
			t.Errorf("%s: return_type = %v, want %q", tc.name, fn.Props["return_type"], tc.want)
		}
	}

	// no_annotation must have no return_type prop.
	noAnn := idx[mod(relFile)+".no_annotation"]
	if noAnn.Props["return_type"] != nil {
		t.Errorf("no_annotation: return_type = %v, want nil", noAnn.Props["return_type"])
	}
}

func TestExtractFile_ReturnType_Complex(t *testing.T) {
	src := `
def get_config() -> dict[str, Any]:
    pass

def find_user() -> Optional[str]:
    pass

def get_items() -> list[str] | None:
    pass
`
	relFile := "svc/service.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	cases := []struct{ name, want string }{
		{mod(relFile) + ".get_config", "dict[str, Any]"},
		{mod(relFile) + ".find_user", "Optional[str]"},
		{mod(relFile) + ".get_items", "list[str] | None"},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q", tc.name)
		}
		if fn.Props["return_type"] != tc.want {
			t.Errorf("%s: return_type = %v, want %q", tc.name, fn.Props["return_type"], tc.want)
		}
	}
}

func TestExtractFile_ReturnType_MultiLine(t *testing.T) {
	// Return type on the closing paren line of a multi-line signature.
	src := `
def create_handler(
    request: Request,
    response: Response,
) -> Optional[str]:
    pass
`
	relFile := "api/handler.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)

	fnName := mod(relFile) + ".create_handler"
	fn, ok := idx[fnName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", fnName, keys(idx))
	}
	if fn.Props["return_type"] != "Optional[str]" {
		t.Errorf("create_handler: return_type = %v, want Optional[str]", fn.Props["return_type"])
	}
}

// --- Phase 1c: Django support ---

func TestExtractFile_DjangoModel(t *testing.T) {
	src := `
from django.db import models

class Order(models.Model):
    total = models.DecimalField(max_digits=10, decimal_places=2)

class UserProfile(models.Model):
    user = models.OneToOneField('User', on_delete=models.CASCADE)
`
	relFile := "shop/models.py"
	result := astExtract(t, relFile, src, true)

	storages := factsByKind(result, facts.KindStorage)
	if len(storages) != 2 {
		t.Fatalf("expected 2 storage facts, got %d: %v", len(storages), storages)
	}

	idx := byName(result)

	// Order → "order" (camelToSnake)
	order, ok := idx["order"]
	if !ok {
		t.Fatalf("missing storage fact %q; keys: %v", "order", keys(idx))
	}
	if order.Props["framework"] != "django" {
		t.Errorf("order: framework = %v, want django", order.Props["framework"])
	}
	if order.Props["storage_kind"] != "table" {
		t.Errorf("order: storage_kind = %v, want table", order.Props["storage_kind"])
	}
	wantClass := mod(relFile) + ".Order"
	if order.Props["class"] != wantClass {
		t.Errorf("order: class = %v, want %q", order.Props["class"], wantClass)
	}

	// UserProfile → "user_profile"
	if _, ok := idx["user_profile"]; !ok {
		t.Errorf("missing storage fact %q; keys: %v", "user_profile", keys(idx))
	}
}

func TestExtractFile_DjangoCBV(t *testing.T) {
	src := `
from rest_framework.views import APIView

class OrderView(APIView):
    def get(self, request):
        pass
`
	relFile := "shop/views.py"
	result := astExtract(t, relFile, src, true)
	idx := byName(result)

	clsName := mod(relFile) + ".OrderView"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", clsName, keys(idx))
	}
	if cls.Props["django_component"] != "view" {
		t.Errorf("OrderView: django_component = %v, want view", cls.Props["django_component"])
	}
	if cls.Props["framework"] != "django" {
		t.Errorf("OrderView: framework = %v, want django", cls.Props["framework"])
	}
}

func TestExtractFile_DRFSerializer(t *testing.T) {
	src := `
from rest_framework import serializers

class OrderSerializer(serializers.ModelSerializer):
    class Meta:
        model = Order
        fields = '__all__'
`
	relFile := "shop/serializers.py"
	result := astExtract(t, relFile, src, true)
	idx := byName(result)

	clsName := mod(relFile) + ".OrderSerializer"
	cls, ok := idx[clsName]
	if !ok {
		t.Fatalf("missing %q; keys: %v", clsName, keys(idx))
	}
	if cls.Props["django_component"] != "serializer" {
		t.Errorf("OrderSerializer: django_component = %v, want serializer", cls.Props["django_component"])
	}
	if cls.Props["framework"] != "django" {
		t.Errorf("OrderSerializer: framework = %v, want django", cls.Props["framework"])
	}
}

func TestExtractFile_DjangoURL(t *testing.T) {
	src := `
from django.urls import path
from . import views

urlpatterns = [
    path('orders/', views.OrderListView.as_view()),
    path('orders/<int:pk>/', views.OrderDetailView.as_view()),
    re_path(r'^legacy/$', views.legacy_view),
]
`
	// File must be named urls.py for Django URL extraction.
	result := astExtract(t, "shop/urls.py", src, true)
	routes := factsByKind(result, facts.KindRoute)

	if len(routes) != 3 {
		t.Fatalf("expected 3 route facts, got %d: %v", len(routes), routes)
	}
	idx := byName(result)

	for _, wantName := range []string{"* orders/", "* orders/<int:pk>/", "* ^legacy/$"} {
		r, ok := idx[wantName]
		if !ok {
			t.Errorf("missing route %q; keys: %v", wantName, keys(idx))
			continue
		}
		if r.Props["framework"] != "django" {
			t.Errorf("%s: framework = %v, want django", wantName, r.Props["framework"])
		}
	}
}

func TestExtractFile_DjangoURL_NonURLsFile(t *testing.T) {
	// Django URL patterns in a file not named urls.py must NOT produce route facts.
	src := `
urlpatterns = [
    path('orders/', views.OrderListView.as_view()),
]
`
	result := astExtract(t, "shop/routing.py", src, true)
	routes := factsByKind(result, facts.KindRoute)
	if len(routes) != 0 {
		t.Errorf("expected no routes in non-urls.py file, got %d", len(routes))
	}
}

func TestExtractFile_DjangoAPIView(t *testing.T) {
	src := `
from rest_framework.decorators import api_view

@api_view(['GET', 'POST'])
def order_list(request):
    pass

@api_view(['GET'])
def order_detail(request, pk):
    pass
`
	relFile := "shop/views.py"
	result := astExtract(t, relFile, src, true)
	routes := factsByKind(result, facts.KindRoute)

	// order_list has GET+POST → 2 routes; order_detail has GET → 1 route.
	if len(routes) != 3 {
		t.Fatalf("expected 3 route facts, got %d: %v", len(routes), routes)
	}

	handlerBase := mod(relFile) + ".order_list"
	idx := byName(result)
	for _, wantName := range []string{
		"GET (view) " + handlerBase,
		"POST (view) " + handlerBase,
	} {
		r, ok := idx[wantName]
		if !ok {
			t.Errorf("missing route %q; keys: %v", wantName, keys(idx))
			continue
		}
		if r.Props["framework"] != "django" {
			t.Errorf("%s: framework = %v, want django", wantName, r.Props["framework"])
		}
	}
}

// --- Helper unit tests ---

func TestCamelToSnake(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Order", "order"},
		{"UserProfile", "user_profile"},
		{"ProductCategory", "product_category"},
		{"Foo", "foo"},
		{"FooBar", "foo_bar"},
	}
	for _, tc := range cases {
		got := camelToSnake(tc.input)
		if got != tc.want {
			t.Errorf("camelToSnake(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDetectDjango(t *testing.T) {
	t.Run("requirements_txt", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("django>=4.2\nrest_framework\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !detectDjango(dir) {
			t.Error("detectDjango should return true for requirements.txt with django")
		}
	})

	t.Run("manage_py", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "manage.py"), []byte("#!/usr/bin/env python\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !detectDjango(dir) {
			t.Error("detectDjango should return true when manage.py is present")
		}
	})

	t.Run("no_django", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("fastapi\nsqlalchemy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if detectDjango(dir) {
			t.Error("detectDjango should return false for non-Django project")
		}
	})
}
