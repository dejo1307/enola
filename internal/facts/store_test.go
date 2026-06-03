package facts

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// --- helpers ---

func makeFact(kind, name, file string, rels ...Relation) Fact {
	return Fact{Kind: kind, Name: name, File: file, Relations: rels}
}

func makeSymbol(name, file, symbolKind string, exported bool) Fact {
	return Fact{
		Kind: KindSymbol,
		Name: name,
		File: file,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    exported,
		},
	}
}

func makeModule(name string) Fact {
	return Fact{Kind: KindModule, Name: name}
}

func makeDep(file, target string) Fact {
	return Fact{
		Kind: KindDependency,
		File: file,
		Relations: []Relation{
			{Kind: RelImports, Target: target},
		},
	}
}

// --- tests ---

func TestAdd_IndexesAllThreeMaps(t *testing.T) {
	s := NewStore()
	f := makeFact(KindSymbol, "pkg.Foo", "pkg/foo.go")
	s.Add(f)

	if got := s.ByKind(KindSymbol); len(got) != 1 || got[0].Name != "pkg.Foo" {
		t.Errorf("ByKind(symbol) = %v, want [pkg.Foo]", got)
	}
	if got := s.ByFile("pkg/foo.go"); len(got) != 1 || got[0].Name != "pkg.Foo" {
		t.Errorf("ByFile(pkg/foo.go) = %v, want [pkg.Foo]", got)
	}
	if got := s.ByName("pkg.Foo"); len(got) != 1 || got[0].Name != "pkg.Foo" {
		t.Errorf("ByName(pkg.Foo) = %v, want [pkg.Foo]", got)
	}
}

func TestAdd_EmptyFileAndNameNotIndexed(t *testing.T) {
	s := NewStore()
	// Fact with empty File and empty Name
	s.Add(Fact{Kind: KindModule})

	if got := s.ByKind(KindModule); len(got) != 1 {
		t.Fatalf("ByKind(module) = %d facts, want 1", len(got))
	}
	// Empty file should not be indexed
	if got := s.ByFile(""); len(got) != 0 {
		t.Errorf("ByFile('') = %d facts, want 0", len(got))
	}
	// Empty name should not be indexed
	if got := s.ByName(""); len(got) != 0 {
		t.Errorf("ByName('') = %d facts, want 0", len(got))
	}
}

func TestQuery_MultiFilter(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("FooBar", "a/foo.go", SymbolFunc, true),
		makeSymbol("BazFoo", "a/baz.go", SymbolStruct, false),
		makeSymbol("Qux", "b/qux.go", SymbolFunc, true),
		makeModule("mod-a"),
		makeFact(KindDependency, "dep1", "a/foo.go", Relation{Kind: RelImports, Target: "fmt"}),
	)

	tests := []struct {
		name    string
		kind    string
		file    string
		qName   string
		relKind string
		want    int
	}{
		{"all empty returns everything", "", "", "", "", 5},
		{"filter by kind=symbol", KindSymbol, "", "", "", 3},
		{"filter by kind=module", KindModule, "", "", "", 1},
		{"filter by file", KindSymbol, "a/foo.go", "", "", 1},
		{"name substring Foo matches FooBar and BazFoo", KindSymbol, "", "Foo", "", 2},
		{"name substring is case-insensitive", KindSymbol, "", "foo", "", 2},
		{"name substring Qux", "", "", "Qux", "", 1},
		{"filter by relKind imports", "", "", "", RelImports, 1},
		{"combined kind+name", KindSymbol, "", "Bar", "", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Query(tt.kind, tt.file, tt.qName, tt.relKind)
			if len(got) != tt.want {
				t.Errorf("Query(%q,%q,%q,%q) returned %d facts, want %d",
					tt.kind, tt.file, tt.qName, tt.relKind, len(got), tt.want)
			}
		})
	}
}

func TestQuery_RelationFilter(t *testing.T) {
	s := NewStore()
	// Fact with both imports and calls relations
	s.Add(Fact{
		Kind: KindDependency,
		Name: "multi-rel",
		File: "a.go",
		Relations: []Relation{
			{Kind: RelImports, Target: "fmt"},
			{Kind: RelCalls, Target: "fmt.Println"},
		},
	})
	// Fact with only declares relation
	s.Add(Fact{
		Kind: KindModule,
		Name: "mod",
		Relations: []Relation{
			{Kind: RelDeclares, Target: "Foo"},
		},
	})

	if got := s.Query("", "", "", RelImports); len(got) != 1 || got[0].Name != "multi-rel" {
		t.Errorf("Query relKind=imports: got %d, want 1 (multi-rel)", len(got))
	}
	if got := s.Query("", "", "", RelCalls); len(got) != 1 || got[0].Name != "multi-rel" {
		t.Errorf("Query relKind=calls: got %d, want 1 (multi-rel)", len(got))
	}
	if got := s.Query("", "", "", RelDeclares); len(got) != 1 || got[0].Name != "mod" {
		t.Errorf("Query relKind=declares: got %d, want 1 (mod)", len(got))
	}
}

func TestJSONL_RoundTrip(t *testing.T) {
	original := NewStore()
	original.Add(
		// Fact with all fields populated
		Fact{
			Kind: KindSymbol,
			Name: "pkg.Foo",
			File: "pkg/foo.go",
			Line: 42,
			Props: map[string]any{
				"symbol_kind": "function",
				"exported":    true,
				"count":       float64(3), // JSON numbers always come back as float64
			},
			Relations: []Relation{
				{Kind: RelCalls, Target: "fmt.Println"},
				{Kind: RelDeclares, Target: "pkg"},
			},
		},
		// Fact with nil Props and nil Relations
		Fact{
			Kind: KindModule,
			Name: "pkg",
			File: "pkg",
		},
		// Fact with empty Props map
		Fact{
			Kind:  KindDependency,
			Name:  "dep",
			File:  "pkg/foo.go",
			Props: map[string]any{},
			Relations: []Relation{
				{Kind: RelImports, Target: "fmt"},
			},
		},
	)

	var buf bytes.Buffer
	if err := original.WriteJSONL(&buf); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}

	restored := NewStore()
	if err := restored.ReadJSONL(&buf); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}

	if restored.Count() != original.Count() {
		t.Fatalf("count mismatch: got %d, want %d", restored.Count(), original.Count())
	}

	origAll := original.All()
	restAll := restored.All()

	for i := range origAll {
		o, r := origAll[i], restAll[i]
		if o.Kind != r.Kind || o.Name != r.Name || o.File != r.File || o.Line != r.Line {
			t.Errorf("fact[%d] basic fields mismatch: %+v vs %+v", i, o, r)
		}
		if len(o.Relations) != len(r.Relations) {
			t.Errorf("fact[%d] relations count: %d vs %d", i, len(o.Relations), len(r.Relations))
		}
		for j := range o.Relations {
			if o.Relations[j] != r.Relations[j] {
				t.Errorf("fact[%d] rel[%d]: %+v vs %+v", i, j, o.Relations[j], r.Relations[j])
			}
		}
	}

	// Verify the Props with type-specific checks (JSON turns all numbers to float64)
	sym := restored.ByKind(KindSymbol)
	if len(sym) != 1 {
		t.Fatalf("expected 1 symbol after round-trip, got %d", len(sym))
	}
	if sk, ok := sym[0].Props["symbol_kind"].(string); !ok || sk != "function" {
		t.Errorf("symbol_kind: got %v, want 'function'", sym[0].Props["symbol_kind"])
	}
	if exp, ok := sym[0].Props["exported"].(bool); !ok || !exp {
		t.Errorf("exported: got %v, want true", sym[0].Props["exported"])
	}
	if cnt, ok := sym[0].Props["count"].(float64); !ok || cnt != 3.0 {
		t.Errorf("count: got %v (type %T), want float64(3)", sym[0].Props["count"], sym[0].Props["count"])
	}
}

func TestJSONL_SkipsEmptyLines(t *testing.T) {
	s := NewStore()
	// JSONL with blank lines and trailing newline
	input := `{"kind":"module","name":"a"}

{"kind":"module","name":"b"}

`
	if err := s.ReadJSONL(strings.NewReader(input)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if s.Count() != 2 {
		t.Errorf("count = %d, want 2 (empty lines should be skipped)", s.Count())
	}
}

func TestJSONL_EmptyStore(t *testing.T) {
	s := NewStore()
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty store should write 0 bytes, got %d", buf.Len())
	}

	restored := NewStore()
	if err := restored.ReadJSONL(&buf); err != nil {
		t.Fatalf("ReadJSONL on empty: %v", err)
	}
	if restored.Count() != 0 {
		t.Errorf("restored count = %d, want 0", restored.Count())
	}
}

func TestClear_ResetsIndexes(t *testing.T) {
	s := NewStore()
	s.Add(
		makeFact(KindSymbol, "Foo", "a.go"),
		makeFact(KindModule, "mod", ""),
	)
	if s.Count() != 2 {
		t.Fatalf("pre-clear count = %d, want 2", s.Count())
	}

	s.Clear()

	if s.Count() != 0 {
		t.Errorf("post-clear Count() = %d, want 0", s.Count())
	}
	if got := s.ByKind(KindSymbol); len(got) != 0 {
		t.Errorf("post-clear ByKind(symbol) = %d, want 0", len(got))
	}
	if got := s.ByName("Foo"); len(got) != 0 {
		t.Errorf("post-clear ByName(Foo) = %d, want 0", len(got))
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("post-clear All() = %d, want 0", len(got))
	}

	// Verify Add works after Clear
	s.Add(makeFact(KindSymbol, "Bar", "b.go"))
	if s.Count() != 1 {
		t.Errorf("post-clear+add Count() = %d, want 1", s.Count())
	}
	if got := s.ByName("Bar"); len(got) != 1 {
		t.Errorf("post-clear+add ByName(Bar) = %d, want 1", len(got))
	}
}

func TestRemoveWhere_FiltersAndRebuildsIndices(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Foo", File: "a.go", Repo: "repoA"},
		Fact{Kind: KindService, Name: "repoA", Repo: "repoA", Props: map[string]any{"synthetic": "crossrepo"}},
		Fact{Kind: KindDependency, Name: "repoA -> repoB", Repo: "repoA", Props: map[string]any{"synthetic": "crossrepo"}},
		Fact{Kind: KindModule, Name: "mod", File: "b.go", Repo: "repoB"},
	)

	removed := s.RemoveWhere(func(f Fact) bool { return f.Props["synthetic"] == "crossrepo" })
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if s.Count() != 2 {
		t.Fatalf("post-remove Count() = %d, want 2", s.Count())
	}

	// Survivors are still indexed correctly.
	if got := s.ByName("Foo"); len(got) != 1 {
		t.Errorf("ByName(Foo) = %d, want 1", len(got))
	}
	if got := s.ByKind(KindModule); len(got) != 1 || got[0].Name != "mod" {
		t.Errorf("ByKind(module) = %+v, want [mod]", got)
	}
	if got := s.ByRepo("repoB"); len(got) != 1 {
		t.Errorf("ByRepo(repoB) = %d, want 1", len(got))
	}

	// Removed facts are gone from all indices.
	if got := s.ByKind(KindService); len(got) != 0 {
		t.Errorf("ByKind(service) = %d, want 0", len(got))
	}
	if got := s.ByName("repoA -> repoB"); len(got) != 0 {
		t.Errorf("ByName(cross-repo dep) = %d, want 0", len(got))
	}
	if got := s.ByRepo("repoA"); len(got) != 1 { // only "Foo" remains in repoA
		t.Errorf("ByRepo(repoA) = %d, want 1", len(got))
	}

	// Graph is invalidated by removal.
	if s.Graph() != nil {
		t.Errorf("Graph() = non-nil after RemoveWhere, want nil")
	}

	// Idempotent: re-adding synthetic facts then removing again is stable.
	s.Add(Fact{Kind: KindService, Name: "repoB", Repo: "repoB", Props: map[string]any{"synthetic": "crossrepo"}})
	if again := s.RemoveWhere(func(f Fact) bool { return f.Props["synthetic"] == "crossrepo" }); again != 1 {
		t.Errorf("second remove = %d, want 1", again)
	}
	if s.Count() != 2 {
		t.Errorf("final Count() = %d, want 2", s.Count())
	}
}

func TestRemoveWhere_NoMatchIsNoOp(t *testing.T) {
	s := NewStore()
	s.Add(makeFact(KindSymbol, "Foo", "a.go"))
	s.BuildGraph()
	if removed := s.RemoveWhere(func(f Fact) bool { return false }); removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if s.Count() != 1 {
		t.Errorf("Count() = %d, want 1", s.Count())
	}
	// A no-op must preserve the existing graph (no rebuild needed).
	if s.Graph() == nil {
		t.Errorf("Graph() = nil after no-op RemoveWhere, want preserved")
	}
}

// --- QueryAdvanced tests ---

func TestQueryAdvanced_MultiKind(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "a.go", SymbolFunc, true),
		makeModule("mod-a"),
		makeDep("a.go", "fmt"),
	)

	results, total := s.QueryAdvanced(QueryOpts{Kinds: []string{KindSymbol, KindModule}})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_SingleAndMultiKindMerge(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "a.go", SymbolFunc, true),
		makeModule("mod-a"),
		makeDep("a.go", "fmt"),
	)

	// Kind="symbol" merged with Kinds=["module"] should match both
	results, total := s.QueryAdvanced(QueryOpts{Kind: KindSymbol, Kinds: []string{KindModule}})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_MultiFile(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "a/foo.go", SymbolFunc, true),
		makeSymbol("Bar", "a/bar.go", SymbolFunc, true),
		makeSymbol("Baz", "b/baz.go", SymbolFunc, true),
	)

	results, total := s.QueryAdvanced(QueryOpts{Files: []string{"a/foo.go", "a/bar.go"}})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_FilePrefix(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "internal/server/server.go", SymbolFunc, true),
		makeSymbol("Bar", "internal/server/handler.go", SymbolFunc, true),
		makeSymbol("Baz", "internal/facts/store.go", SymbolFunc, true),
	)

	results, total := s.QueryAdvanced(QueryOpts{FilePrefix: "internal/server"})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_FileAndFilePrefixCombine(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "internal/server/server.go", SymbolFunc, true),
		makeSymbol("Bar", "internal/facts/store.go", SymbolFunc, true),
		makeSymbol("Baz", "cmd/main.go", SymbolFunc, true),
	)

	// File="cmd/main.go" OR FilePrefix="internal/server" should match 2
	results, total := s.QueryAdvanced(QueryOpts{File: "cmd/main.go", FilePrefix: "internal/server"})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_ExactNames(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("pkg.Foo", "a.go", SymbolFunc, true),
		makeSymbol("pkg.Bar", "a.go", SymbolFunc, true),
		makeSymbol("pkg.Baz", "a.go", SymbolFunc, true),
	)

	results, total := s.QueryAdvanced(QueryOpts{Names: []string{"pkg.Foo", "pkg.Bar"}})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_SubstringAndExactNamesCombine(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("pkg.FooBar", "a.go", SymbolFunc, true),
		makeSymbol("pkg.Baz", "a.go", SymbolFunc, true),
		makeSymbol("pkg.Qux", "a.go", SymbolFunc, true),
	)

	// Name="Foo" (substring) OR Names=["pkg.Qux"] (exact) should match FooBar and Qux
	results, total := s.QueryAdvanced(QueryOpts{Name: "Foo", Names: []string{"pkg.Qux"}})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_PropFilterBeforeLimit(t *testing.T) {
	s := NewStore()
	// Add 5 symbols: 3 exported, 2 not exported
	s.Add(
		makeSymbol("A", "a.go", SymbolFunc, true),
		makeSymbol("B", "a.go", SymbolFunc, false),
		makeSymbol("C", "a.go", SymbolFunc, true),
		makeSymbol("D", "a.go", SymbolFunc, false),
		makeSymbol("E", "a.go", SymbolFunc, true),
	)

	// With limit=2 and prop filter, prop filter is applied first
	results, total := s.QueryAdvanced(QueryOpts{
		Prop:      "exported",
		PropValue: "true",
		Limit:     2,
	})
	if total != 3 {
		t.Errorf("total = %d, want 3 (all exported)", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2 (limited)", len(results))
	}
}

func TestQueryAdvanced_Pagination(t *testing.T) {
	s := NewStore()
	for i := 0; i < 10; i++ {
		s.Add(makeFact(KindSymbol, fmt.Sprintf("sym%d", i), "a.go"))
	}

	// Page 1: offset=0, limit=3
	r1, total := s.QueryAdvanced(QueryOpts{Limit: 3})
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(r1) != 3 {
		t.Errorf("page1 len = %d, want 3", len(r1))
	}

	// Page 2: offset=3, limit=3
	r2, _ := s.QueryAdvanced(QueryOpts{Offset: 3, Limit: 3})
	if len(r2) != 3 {
		t.Errorf("page2 len = %d, want 3", len(r2))
	}
	if r1[0].Name == r2[0].Name {
		t.Error("page1 and page2 should return different facts")
	}

	// Beyond end: offset=9, limit=3
	r3, _ := s.QueryAdvanced(QueryOpts{Offset: 9, Limit: 3})
	if len(r3) != 1 {
		t.Errorf("last page len = %d, want 1", len(r3))
	}

	// Past end: offset=20
	r4, _ := s.QueryAdvanced(QueryOpts{Offset: 20, Limit: 3})
	if len(r4) != 0 {
		t.Errorf("past end len = %d, want 0", len(r4))
	}
}

func TestQueryAdvanced_LimitClamping(t *testing.T) {
	s := NewStore()
	for i := 0; i < 200; i++ {
		s.Add(makeFact(KindSymbol, fmt.Sprintf("sym%d", i), "a.go"))
	}

	// Default limit (0) should return 100
	r1, _ := s.QueryAdvanced(QueryOpts{})
	if len(r1) != 100 {
		t.Errorf("default limit: got %d, want 100", len(r1))
	}

	// Max limit (>500) should be clamped to 500
	r2, _ := s.QueryAdvanced(QueryOpts{Limit: 1000})
	if len(r2) != 200 {
		t.Errorf("clamped limit: got %d, want 200 (all facts < 500)", len(r2))
	}
}

func TestQueryAdvanced_EmptyFilters(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "a.go", SymbolFunc, true),
		makeModule("mod"),
	)

	results, total := s.QueryAdvanced(QueryOpts{})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
}

func TestQueryAdvanced_CrossDimensionAND(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("Foo", "a.go", SymbolFunc, true),
		makeSymbol("Bar", "b.go", SymbolFunc, true),
		makeModule("mod"),
	)

	// Kind=symbol AND File=a.go should return only Foo
	results, total := s.QueryAdvanced(QueryOpts{Kind: KindSymbol, File: "a.go"})
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 || results[0].Name != "Foo" {
		t.Errorf("expected only Foo, got %v", results)
	}
}

func TestLookupByExactName(t *testing.T) {
	s := NewStore()
	s.Add(
		makeFact(KindSymbol, "pkg.Foo", "a.go"),
		makeFact(KindSymbol, "pkg.FooBar", "b.go"),
	)

	// Exact lookup should only return "pkg.Foo", not "pkg.FooBar"
	got := s.LookupByExactName("pkg.Foo")
	if len(got) != 1 {
		t.Errorf("LookupByExactName(pkg.Foo) = %d, want 1", len(got))
	}
	if len(got) > 0 && got[0].Name != "pkg.Foo" {
		t.Errorf("expected pkg.Foo, got %s", got[0].Name)
	}

	// Non-existent name
	got = s.LookupByExactName("pkg.Nope")
	if len(got) != 0 {
		t.Errorf("LookupByExactName(pkg.Nope) = %d, want 0", len(got))
	}
}

func TestReverseLookup(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "A", File: "a.go", Relations: []Relation{
			{Kind: RelCalls, Target: "B"},
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindSymbol, Name: "B", File: "b.go", Relations: []Relation{
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindSymbol, Name: "C", File: "c.go"},
		Fact{Kind: KindDependency, Name: "dep", File: "a.go", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
	)

	// Who calls B?
	callers := s.ReverseLookup("B", RelCalls)
	if len(callers) != 1 || callers[0].Name != "A" {
		t.Errorf("ReverseLookup(B, calls) = %v, want [A]", callers)
	}

	// Who references B with any relation?
	all := s.ReverseLookup("B", "")
	if len(all) != 2 {
		t.Errorf("ReverseLookup(B, '') = %d, want 2 (A calls, dep imports)", len(all))
	}

	// Who calls C?
	cCallers := s.ReverseLookup("C", RelCalls)
	if len(cCallers) != 2 {
		t.Errorf("ReverseLookup(C, calls) = %d, want 2 (A and B)", len(cCallers))
	}

	// Non-existent target
	none := s.ReverseLookup("Z", "")
	if len(none) != 0 {
		t.Errorf("ReverseLookup(Z, '') = %d, want 0", len(none))
	}
}

// --- Repo / multi-repo tests ---

func TestByRepo(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Foo", File: "lib/foo.rb", Repo: "go-service"},
		Fact{Kind: KindSymbol, Name: "Bar", File: "lib/bar.rb", Repo: "ruby-monolith"},
		Fact{Kind: KindSymbol, Name: "Baz", File: "lib/baz.rb"}, // no repo
	)

	got := s.ByRepo("go-service")
	if len(got) != 1 || got[0].Name != "Foo" {
		t.Errorf("ByRepo(go-service) = %v, want [Foo]", got)
	}
	got = s.ByRepo("ruby-monolith")
	if len(got) != 1 || got[0].Name != "Bar" {
		t.Errorf("ByRepo(ruby-monolith) = %v, want [Bar]", got)
	}
	got = s.ByRepo("nonexistent")
	if len(got) != 0 {
		t.Errorf("ByRepo(nonexistent) = %d, want 0", len(got))
	}
}

func TestQueryAdvanced_RepoFilter(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Foo", File: "go-service/lib/foo.rb", Repo: "go-service"},
		Fact{Kind: KindSymbol, Name: "Bar", File: "ruby-monolith/lib/bar.rb", Repo: "ruby-monolith"},
		Fact{Kind: KindSymbol, Name: "Baz", File: "go-service/lib/baz.rb", Repo: "go-service"},
	)

	results, total := s.QueryAdvanced(QueryOpts{Repo: "go-service"})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	for _, r := range results {
		if r.Repo != "go-service" {
			t.Errorf("expected repo go-service, got %q", r.Repo)
		}
	}

	results, total = s.QueryAdvanced(QueryOpts{Repo: "ruby-monolith"})
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 || results[0].Name != "Bar" {
		t.Errorf("expected [Bar], got %v", results)
	}
}

func TestQueryAdvanced_RepoAndFilePrefixCombined(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Foo", File: "go-service/lib/foo.rb", Repo: "go-service"},
		Fact{Kind: KindSymbol, Name: "Bar", File: "go-service/app/bar.rb", Repo: "go-service"},
		Fact{Kind: KindSymbol, Name: "Baz", File: "ruby-monolith/lib/baz.rb", Repo: "ruby-monolith"},
	)

	// Repo AND FilePrefix
	results, total := s.QueryAdvanced(QueryOpts{Repo: "go-service", FilePrefix: "go-service/lib"})
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(results) != 1 || results[0].Name != "Foo" {
		t.Errorf("expected [Foo], got %v", results)
	}
}

func TestTagRange(t *testing.T) {
	s := NewStore()
	// Pre-existing facts (from repo A)
	s.Add(
		makeSymbol("Existing", "a/foo.go", SymbolFunc, true),
	)

	startIdx := s.Count() // 1

	// New facts (from repo B)
	s.Add(
		makeSymbol("New1", "lib/new1.rb", SymbolFunc, true),
		makeSymbol("New2", "app/new2.rb", SymbolFunc, true),
	)

	s.TagRange(startIdx, "repo-b", "repo-b/")

	// Existing fact should be unchanged
	existing := s.ByName("Existing")
	if len(existing) != 1 || existing[0].Repo != "" || existing[0].File != "a/foo.go" {
		t.Errorf("existing fact mutated: repo=%q file=%q", existing[0].Repo, existing[0].File)
	}

	// New facts should be tagged and prefixed
	new1 := s.ByName("New1")
	if len(new1) != 1 {
		t.Fatalf("New1 not found")
	}
	if new1[0].Repo != "repo-b" {
		t.Errorf("New1.Repo = %q, want repo-b", new1[0].Repo)
	}
	if new1[0].File != "repo-b/lib/new1.rb" {
		t.Errorf("New1.File = %q, want repo-b/lib/new1.rb", new1[0].File)
	}

	new2 := s.ByName("New2")
	if len(new2) != 1 {
		t.Fatalf("New2 not found")
	}
	if new2[0].File != "repo-b/app/new2.rb" {
		t.Errorf("New2.File = %q, want repo-b/app/new2.rb", new2[0].File)
	}

	// byFile index should be updated
	byOldFile := s.ByFile("lib/new1.rb")
	if len(byOldFile) != 0 {
		t.Error("old file path should no longer be indexed")
	}
	byNewFile := s.ByFile("repo-b/lib/new1.rb")
	if len(byNewFile) != 1 {
		t.Error("new file path should be indexed")
	}

	// byRepo index should be populated
	repoFacts := s.ByRepo("repo-b")
	if len(repoFacts) != 2 {
		t.Errorf("ByRepo(repo-b) = %d, want 2", len(repoFacts))
	}
}

func TestTagRange_FilePrefixQuery(t *testing.T) {
	s := NewStore()
	// Simulate multi-repo: two repos tagged differently
	s.Add(makeSymbol("A", "lib/a.rb", SymbolFunc, true))
	s.TagRange(0, "ruby-monolith", "ruby-monolith/")

	start := s.Count()
	s.Add(makeSymbol("B", "lib/b.rb", SymbolFunc, true))
	s.TagRange(start, "pricing", "pricing/")

	// FilePrefix should distinguish repos
	coreResults, _ := s.QueryAdvanced(QueryOpts{FilePrefix: "ruby-monolith/"})
	if len(coreResults) != 1 || coreResults[0].Name != "A" {
		t.Errorf("ruby-monolith prefix: got %v, want [A]", coreResults)
	}

	pricingResults, _ := s.QueryAdvanced(QueryOpts{FilePrefix: "pricing/"})
	if len(pricingResults) != 1 || pricingResults[0].Name != "B" {
		t.Errorf("pricing prefix: got %v, want [B]", pricingResults)
	}

	// Both repos returned without filter
	all, total := s.QueryAdvanced(QueryOpts{})
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(all) != 2 {
		t.Errorf("all = %d, want 2", len(all))
	}
}

func TestTagUntagged_UntaggedFacts(t *testing.T) {
	s := NewStore()
	// Untagged facts (from old single-repo mode without SetRepoRange)
	s.Add(
		makeSymbol("A", "lib/a.rb", SymbolFunc, true),
		makeSymbol("B", "lib/b.rb", SymbolFunc, true),
	)
	// Already tagged fact from a different repo
	s.Add(Fact{Kind: KindSymbol, Name: "C", File: "other/c.rb", Repo: "other-repo",
		Props: map[string]any{"symbol_kind": SymbolFunc, "exported": true}})

	prefixed := s.TagUntagged("ruby-monolith", "ruby-monolith/")
	if prefixed != 2 {
		t.Errorf("TagUntagged returned %d, want 2 (only ruby-monolith facts prefixed)", prefixed)
	}

	// A and B should have Repo set and files prefixed
	a := s.ByName("A")
	if len(a) != 1 || a[0].Repo != "ruby-monolith" || a[0].File != "ruby-monolith/lib/a.rb" {
		t.Errorf("A: Repo=%q File=%q, want ruby-monolith, ruby-monolith/lib/a.rb", a[0].Repo, a[0].File)
	}

	// C should be unchanged (different repo)
	c := s.ByName("C")
	if len(c) != 1 || c[0].Repo != "other-repo" || c[0].File != "other/c.rb" {
		t.Errorf("C: Repo=%q File=%q, want other-repo, other/c.rb", c[0].Repo, c[0].File)
	}

	// byFile index should be updated
	if got := s.ByFile("lib/a.rb"); len(got) != 0 {
		t.Error("old file path lib/a.rb should no longer be indexed")
	}
	if got := s.ByFile("ruby-monolith/lib/a.rb"); len(got) != 1 {
		t.Error("new file path ruby-monolith/lib/a.rb should be indexed")
	}

	// Running again should prefix 0 (already prefixed)
	prefixed = s.TagUntagged("ruby-monolith", "ruby-monolith/")
	if prefixed != 0 {
		t.Errorf("second TagUntagged returned %d, want 0", prefixed)
	}
}

func TestTagUntagged_AlreadyRepoTagged(t *testing.T) {
	s := NewStore()
	// Facts that already have Repo set (from SetRepoRange in non-append mode)
	// but file paths are NOT prefixed yet.
	s.Add(
		Fact{Kind: KindSymbol, Name: "A", File: "lib/a.rb", Repo: "ruby-monolith",
			Props: map[string]any{"symbol_kind": SymbolFunc, "exported": true}},
		Fact{Kind: KindSymbol, Name: "B", File: "lib/b.rb", Repo: "ruby-monolith",
			Props: map[string]any{"symbol_kind": SymbolFunc, "exported": true}},
	)

	// TagUntagged should still prefix their file paths
	prefixed := s.TagUntagged("ruby-monolith", "ruby-monolith/")
	if prefixed != 2 {
		t.Errorf("TagUntagged returned %d, want 2 (files need prefixing)", prefixed)
	}

	a := s.ByName("A")
	if len(a) != 1 || a[0].File != "ruby-monolith/lib/a.rb" {
		t.Errorf("A.File = %q, want ruby-monolith/lib/a.rb", a[0].File)
	}

	// byRepo index should not have duplicates
	coreFacts := s.ByRepo("ruby-monolith")
	if len(coreFacts) != 2 {
		t.Errorf("ByRepo(ruby-monolith) = %d, want 2", len(coreFacts))
	}
}

func TestSetRepoRange(t *testing.T) {
	s := NewStore()
	s.Add(
		makeSymbol("A", "lib/a.rb", SymbolFunc, true),
		makeSymbol("B", "lib/b.rb", SymbolFunc, true),
	)

	s.SetRepoRange(0, "myrepo")

	a := s.ByName("A")
	if len(a) != 1 || a[0].Repo != "myrepo" {
		t.Errorf("A.Repo = %q, want myrepo", a[0].Repo)
	}
	// File should be unchanged (no prefixing in SetRepoRange)
	if a[0].File != "lib/a.rb" {
		t.Errorf("A.File = %q, want lib/a.rb (should not be prefixed)", a[0].File)
	}

	// byRepo index should work
	repoFacts := s.ByRepo("myrepo")
	if len(repoFacts) != 2 {
		t.Errorf("ByRepo(myrepo) = %d, want 2", len(repoFacts))
	}

	// Running SetRepoRange again should not double-index
	s.SetRepoRange(0, "myrepo")
	repoFacts = s.ByRepo("myrepo")
	if len(repoFacts) != 2 {
		t.Errorf("after second SetRepoRange: ByRepo(myrepo) = %d, want 2", len(repoFacts))
	}
}

func TestClear_ResetsRepoIndex(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "Foo", File: "a.go", Repo: "myrepo"})
	if got := s.ByRepo("myrepo"); len(got) != 1 {
		t.Fatalf("pre-clear ByRepo = %d, want 1", len(got))
	}
	s.Clear()
	if got := s.ByRepo("myrepo"); len(got) != 0 {
		t.Errorf("post-clear ByRepo = %d, want 0", len(got))
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStore()
	const n = 100
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Add(makeFact(KindSymbol, "sym", "file.go"))
		}(i)
	}

	// Concurrent readers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.All()
			_ = s.ByKind(KindSymbol)
			_ = s.Query(KindSymbol, "", "", "")
			_ = s.Count()
		}()
	}

	wg.Wait()

	if got := s.Count(); got != n {
		t.Errorf("after concurrent adds: Count() = %d, want %d", got, n)
	}
}
