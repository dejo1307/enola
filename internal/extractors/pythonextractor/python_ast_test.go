package pythonextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// astExtract is a helper that writes src to a temp file and runs extractFileAST.
func astExtract(t *testing.T, filename, src string, isDjango bool) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return extractFileAST([]byte(src), filename, isDjango)
}

// relsByKind returns all relations of a given kind from a fact.
func relsByKind(f facts.Fact, kind string) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == kind {
			out = append(out, r.Target)
		}
	}
	return out
}

// --- Call graph tests ---

func TestAST_SameModuleFunctionCall(t *testing.T) {
	src := `
def helper():
    pass

def main():
    helper()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	mainFact, ok := idx["svc.main"]
	if !ok {
		t.Fatalf("missing svc.main; keys: %v", keys(idx))
	}
	calls := relsByKind(mainFact, facts.RelCalls)
	if len(calls) == 0 {
		t.Fatal("svc.main: expected RelCalls to svc.helper, got none")
	}
	found := false
	for _, c := range calls {
		if c == "svc.helper" {
			found = true
		}
	}
	if !found {
		t.Errorf("svc.main: RelCalls = %v, want svc.helper", calls)
	}
}

func TestAST_SelfMethodCall(t *testing.T) {
	src := `
class Service:
    def _do_work(self):
        pass

    def run(self):
        self._do_work()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	runFact, ok := idx["svc.Service.run"]
	if !ok {
		t.Fatalf("missing svc.Service.run; keys: %v", keys(idx))
	}
	calls := relsByKind(runFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.Service._do_work" {
			found = true
		}
	}
	if !found {
		t.Errorf("Service.run: RelCalls = %v, want svc.Service._do_work", calls)
	}
}

func TestAST_Constructor_RelInstantiates(t *testing.T) {
	src := `
class Order:
    pass

def create():
    o = Order()
    return o
`
	result := astExtract(t, "models.py", src, false)
	idx := byName(result)

	createFact, ok := idx["models.create"]
	if !ok {
		t.Fatalf("missing models.create; keys: %v", keys(idx))
	}
	insts := relsByKind(createFact, facts.RelInstantiates)
	found := false
	for _, i := range insts {
		if i == "Order" {
			found = true
		}
	}
	if !found {
		t.Errorf("create: RelInstantiates = %v, want Order", insts)
	}
}

func TestAST_NoEdgeForBuiltins(t *testing.T) {
	src := `
def process(items):
    result = list(map(str, items))
    print(len(result))
    return sorted(result)
`
	result := astExtract(t, "util.py", src, false)
	idx := byName(result)

	fn, ok := idx["util.process"]
	if !ok {
		t.Fatalf("missing util.process")
	}
	calls := relsByKind(fn, facts.RelCalls)
	for _, c := range calls {
		if c == "util.list" || c == "util.print" || c == "util.sorted" || c == "util.map" || c == "util.str" || c == "util.len" {
			t.Errorf("process: should not emit call edge to builtin, got %q", c)
		}
	}
}

func TestAST_ReturnType_FromAST(t *testing.T) {
	// tree-sitter reads the return type node directly — no regex needed.
	src := `
def get_user(user_id: int) -> Optional[str]:
    pass

def create_order(
    items: list,
    total: float,
) -> dict[str, Any]:
    pass
`
	result := astExtract(t, "api.py", src, false)
	idx := byName(result)

	cases := []struct{ name, want string }{
		{"api.get_user", "Optional[str]"},
		{"api.create_order", "dict[str, Any]"},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", tc.name, keys(idx))
		}
		if fn.Props["return_type"] != tc.want {
			t.Errorf("%s: return_type = %v, want %q", tc.name, fn.Props["return_type"], tc.want)
		}
	}
}

func TestAST_NestedClass(t *testing.T) {
	src := `
class Outer:
    class Inner:
        def method(self):
            pass
`
	result := astExtract(t, "nested.py", src, false)
	idx := byName(result)

	if _, ok := idx["nested.Outer"]; !ok {
		t.Errorf("missing nested.Outer; keys: %v", keys(idx))
	}
	if _, ok := idx["nested.Outer.Inner"]; !ok {
		t.Errorf("missing nested.Outer.Inner; keys: %v", keys(idx))
	}
	if _, ok := idx["nested.Outer.Inner.method"]; !ok {
		t.Errorf("missing nested.Outer.Inner.method; keys: %v", keys(idx))
	}
}

func TestAST_AsyncFunction(t *testing.T) {
	src := `
async def fetch_data(url: str) -> bytes:
    pass
`
	result := astExtract(t, "client.py", src, false)
	idx := byName(result)

	fn, ok := idx["client.fetch_data"]
	if !ok {
		t.Fatalf("missing client.fetch_data")
	}
	if fn.Props["async"] != true {
		t.Errorf("fetch_data: async = %v, want true", fn.Props["async"])
	}
	if fn.Props["return_type"] != "bytes" {
		t.Errorf("fetch_data: return_type = %v, want bytes", fn.Props["return_type"])
	}
}

func TestAST_DecoratorProps(t *testing.T) {
	src := `
class Repo:
    @staticmethod
    def from_dict(d):
        pass

    @classmethod
    def create(cls):
        pass

    @property
    def name(self):
        return self._name
`
	result := astExtract(t, "repo.py", src, false)
	idx := byName(result)

	cases := []struct {
		name string
		prop string
		want any
	}{
		{"repo.Repo.from_dict", "static", true},
		{"repo.Repo.create", "class_method", true},
		{"repo.Repo.name", "property", true},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", tc.name, keys(idx))
		}
		if fn.Props[tc.prop] != tc.want {
			t.Errorf("%s: %s = %v, want %v", tc.name, tc.prop, fn.Props[tc.prop], tc.want)
		}
	}
}

func TestAST_SQLAlchemyTable(t *testing.T) {
	src := `
from sqlalchemy import Column, Integer, String
from sqlalchemy.orm import DeclarativeBase

class Base(DeclarativeBase):
    pass

class Product(Base):
    __tablename__ = "products"
    id = Column(Integer, primary_key=True)
    name = Column(String)
`
	result := astExtract(t, "models.py", src, false)
	storages := factsByKind(result, facts.KindStorage)
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage fact, got %d: %v", len(storages), storages)
	}
	if storages[0].Name != "products" {
		t.Errorf("storage name = %q, want products", storages[0].Name)
	}
	if storages[0].Props["framework"] != "sqlalchemy" {
		t.Errorf("storage framework = %v, want sqlalchemy", storages[0].Props["framework"])
	}
}

func TestAST_ImportEdges(t *testing.T) {
	src := `
import os
from pathlib import Path
from . import utils
`
	result := astExtract(t, "mymod.py", src, false)
	deps := factsByKind(result, facts.KindDependency)
	if len(deps) < 3 {
		t.Errorf("expected >= 3 dependency facts, got %d", len(deps))
	}
	// Each dep must carry a RelImports relation.
	for _, d := range deps {
		found := false
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports {
				found = true
			}
		}
		if !found {
			t.Errorf("dependency %q missing RelImports relation", d.Name)
		}
	}
}
