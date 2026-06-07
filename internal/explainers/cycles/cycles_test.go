package cycles

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func sortedSCC(scc []string) []string {
	s := make([]string, len(scc))
	copy(s, scc)
	sort.Strings(s)
	return s
}

func makeStore(modules []string, deps map[string][]string) *facts.Store {
	s := facts.NewStore()
	for _, m := range modules {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind: facts.KindDependency,
				File: src + "/file.go",
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: tgt},
				},
			})
		}
	}
	return s
}

// --- Tarjan's SCC tests ---

func TestTarjanSCC_KnownGraphs(t *testing.T) {
	tests := []struct {
		name      string
		graph     map[string][]string
		wantCycleCount int // SCCs with size > 1
		wantCycleSizes []int // sorted sizes of non-trivial SCCs
	}{
		{
			name:           "empty graph",
			graph:          map[string][]string{},
			wantCycleCount: 0,
		},
		{
			name:           "single node no edges",
			graph:          map[string][]string{"A": nil},
			wantCycleCount: 0,
		},
		{
			name:           "simple cycle A<->B",
			graph:          map[string][]string{"A": {"B"}, "B": {"A"}},
			wantCycleCount: 1,
			wantCycleSizes: []int{2},
		},
		{
			name:           "triangle A->B->C->A",
			graph:          map[string][]string{"A": {"B"}, "B": {"C"}, "C": {"A"}},
			wantCycleCount: 1,
			wantCycleSizes: []int{3},
		},
		{
			name: "two disjoint cycles",
			graph: map[string][]string{
				"A": {"B"}, "B": {"A"},
				"C": {"D"}, "D": {"C"},
			},
			wantCycleCount: 2,
			wantCycleSizes: []int{2, 2},
		},
		{
			name:           "chain no cycle A->B->C",
			graph:          map[string][]string{"A": {"B"}, "B": {"C"}, "C": nil},
			wantCycleCount: 0,
		},
		{
			name: "complex graph: cycle with tail",
			graph: map[string][]string{
				"A": {"B"}, "B": {"C"}, "C": {"A", "D"}, "D": nil,
			},
			wantCycleCount: 1,
			wantCycleSizes: []int{3},
		},
		{
			name: "two cycles sharing a node",
			graph: map[string][]string{
				"A": {"B"}, "B": {"A", "C"}, "C": {"B"},
			},
			wantCycleCount: 1,
			wantCycleSizes: []int{3}, // A, B, C are all in one SCC
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sccs := tarjanSCC(tt.graph)
			var cycles [][]string
			for _, scc := range sccs {
				if len(scc) > 1 {
					cycles = append(cycles, scc)
				}
			}
			if len(cycles) != tt.wantCycleCount {
				t.Errorf("got %d cycles, want %d. SCCs: %v", len(cycles), tt.wantCycleCount, sccs)
				return
			}
			if tt.wantCycleSizes != nil {
				gotSizes := make([]int, len(cycles))
				for i, c := range cycles {
					gotSizes[i] = len(c)
				}
				sort.Ints(gotSizes)
				sort.Ints(tt.wantCycleSizes)
				if len(gotSizes) != len(tt.wantCycleSizes) {
					t.Errorf("cycle sizes: got %v, want %v", gotSizes, tt.wantCycleSizes)
				} else {
					for i := range gotSizes {
						if gotSizes[i] != tt.wantCycleSizes[i] {
							t.Errorf("cycle sizes[%d]: got %d, want %d", i, gotSizes[i], tt.wantCycleSizes[i])
						}
					}
				}
			}
		})
	}
}

func TestTarjanSCC_SelfLoop(t *testing.T) {
	graph := map[string][]string{"A": {"A"}}
	sccs := tarjanSCC(graph)
	// Self-loop creates an SCC of size 1 — should not panic
	for _, scc := range sccs {
		if len(scc) > 1 {
			t.Errorf("self-loop should not produce SCC > 1, got %v", scc)
		}
	}
}

// --- isExternalImport tests ---

func TestIsExternalImport(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"fmt", true},                    // Go stdlib (no slash)
		{"react", true},                  // npm package (no slash)
		{"github.com/foo/bar", true},     // Go third-party (has dot)
		{"./relative", false},            // relative import
		{"../parent", false},             // parent relative import
		{"/absolute/path", false},        // absolute path
		{"internal/pkg", false},          // internal module (has slash, no dot)
		{"src/components", false},        // internal path (has slash, no dot)
		// Known edge case: @types/node has slash but is npm-external.
		// Current implementation returns false (treats as internal) because
		// it has "/" and no ".". This documents the behavior.
		{"@types/node", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isExternalImport(tt.path); got != tt.want {
				t.Errorf("isExternalImport(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// --- resolveRelativeImport tests ---

func TestResolveRelativeImport(t *testing.T) {
	tests := []struct {
		source string
		target string
		want   string
	}{
		{"src/components", "./utils", "src/components/utils"},
		{"src/components", "../hooks", "src/hooks"},
		{"src/components", "../../lib", "lib"},
		{"src/deep/nested", "../../../top", "top"},
		{"src", "./foo", "src/foo"},
		// "." as source: the dot stays in the joined result
		{".", "./foo", "./foo"},
		// Going up past root produces empty path
		{"src", "../../above", "above"},
	}

	for _, tt := range tests {
		t.Run(tt.source+"→"+tt.target, func(t *testing.T) {
			got := resolveRelativeImport(tt.source, tt.target)
			if got != tt.want {
				t.Errorf("resolveRelativeImport(%q, %q) = %q, want %q",
					tt.source, tt.target, got, tt.want)
			}
		})
	}
}

// --- buildDependencyGraph tests ---

func TestBuildDependencyGraph(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b", "fmt", "github.com/foo/bar"},
			"src/b": {"src/c", "react"},
		},
	)

	graph := buildDependencyGraph(store)

	// src/a should only have edge to src/b (fmt and github.com/foo/bar are external)
	if edges, ok := graph["src/a"]; !ok {
		t.Error("src/a not in graph")
	} else {
		if len(edges) != 1 || edges[0] != "src/b" {
			t.Errorf("src/a edges = %v, want [src/b]", edges)
		}
	}

	// src/b should only have edge to src/c (react is external)
	if edges := graph["src/b"]; len(edges) != 1 || edges[0] != "src/c" {
		t.Errorf("src/b edges = %v, want [src/c]", edges)
	}
}

// --- Integration tests for Explain ---

func TestExplain_NoCycles(t *testing.T) {
	// Use paths with slashes so isExternalImport treats them as internal
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for acyclic graph, got %d: %+v", len(insights), insights)
	}
}

func TestExplain_WithCycle(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/c"},
			"src/c": {"src/a"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 cycle insight, got %d", len(insights))
	}

	insight := insights[0]
	if insight.Confidence != 1.0 {
		t.Errorf("confidence = %f, want 1.0", insight.Confidence)
	}
	if len(insight.Evidence) != 3 {
		t.Errorf("evidence count = %d, want 3 (one per module in cycle)", len(insight.Evidence))
	}
	// Verify all three modules appear in evidence
	evidenceModules := make(map[string]bool)
	for _, ev := range insight.Evidence {
		evidenceModules[ev.Fact] = true
	}
	for _, mod := range []string{"src/a", "src/b", "src/c"} {
		if !evidenceModules[mod] {
			t.Errorf("module %q missing from cycle evidence", mod)
		}
	}
}

func TestExplain_MultipleCycles(t *testing.T) {
	store := makeStore(
		[]string{"src/a", "src/b", "src/c", "src/d"},
		map[string][]string{
			"src/a": {"src/b"},
			"src/b": {"src/a"},
			"src/c": {"src/d"},
			"src/d": {"src/c"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 2 {
		t.Errorf("expected 2 cycle insights for 2 disjoint cycles, got %d", len(insights))
	}
}
