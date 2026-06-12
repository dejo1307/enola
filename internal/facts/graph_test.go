package facts

import (
	"reflect"
	"testing"
)

// buildTestGraph creates a graph from a set of facts for testing.
// The topology is:
//
//	A --calls--> B --calls--> C --calls--> D
//	A --imports-> E
//	E --calls--> C
//	F (disconnected)
func buildTestGraph() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "A", File: "a.go", Line: 1, Relations: []Relation{
			{Kind: RelCalls, Target: "B"},
			{Kind: RelImports, Target: "E"},
		}},
		Fact{Kind: KindSymbol, Name: "B", File: "b.go", Line: 10, Relations: []Relation{
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindModule, Name: "C", File: "c.go", Line: 20, Relations: []Relation{
			{Kind: RelCalls, Target: "D"},
		}},
		Fact{Kind: KindSymbol, Name: "D", File: "d.go", Line: 30},
		Fact{Kind: KindModule, Name: "E", File: "e.go", Line: 40, Relations: []Relation{
			{Kind: RelCalls, Target: "C"},
		}},
		Fact{Kind: KindSymbol, Name: "F", File: "f.go", Line: 50}, // disconnected
	)
	s.BuildGraph()
	return s.Graph(), s
}

// buildCyclicGraph creates a graph with a cycle: A -> B -> C -> A
func buildCyclicGraph() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "A", File: "a.go", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		Fact{Kind: KindModule, Name: "B", File: "b.go", Relations: []Relation{
			{Kind: RelImports, Target: "C"},
		}},
		Fact{Kind: KindModule, Name: "C", File: "c.go", Relations: []Relation{
			{Kind: RelImports, Target: "A"},
		}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestNewGraph_BuildsAdjacencyLists(t *testing.T) {
	g, _ := buildTestGraph()

	if g.NodeCount() != 6 {
		t.Errorf("NodeCount = %d, want 6", g.NodeCount())
	}
	if g.EdgeCount() != 5 {
		t.Errorf("EdgeCount = %d, want 5", g.EdgeCount())
	}

	// Check forward adjacency for A
	fwd := g.Forward()
	aEdges := fwd["A"]
	if len(aEdges) != 2 {
		t.Errorf("A forward edges = %d, want 2", len(aEdges))
	}

	// Check reverse adjacency for C (B and E both call C)
	rev := g.Reverse()
	cEdges := rev["C"]
	if len(cEdges) != 2 {
		t.Errorf("C reverse edges = %d, want 2", len(cEdges))
	}
}

func TestTraverse_ForwardFromA(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 10, 100)

	// A -> B -> C -> D, A -> E -> C (C already visited)
	// Should visit: A, B, C, D, E
	if result.Stats.NodesVisited != 5 {
		t.Errorf("NodesVisited = %d, want 5", result.Stats.NodesVisited)
	}

	// Nodes in result (including start)
	names := nodeNames(result.Nodes)
	for _, want := range []string{"A", "B", "C", "D", "E"} {
		if !contains(names, want) {
			t.Errorf("missing node %q in traverse result", want)
		}
	}
	if contains(names, "F") {
		t.Error("F should not be reachable from A")
	}
}

func TestTraverse_ReverseFromD(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("D", "reverse", nil, nil, 10, 100)

	// D is called by C, C is called by B and E, B is called by A, E is imported by A
	// Reverse from D: D <- C <- B <- A, C <- E <- A (A already visited)
	names := nodeNames(result.Nodes)
	for _, want := range []string{"D", "C", "B", "A", "E"} {
		if !contains(names, want) {
			t.Errorf("missing node %q in reverse traverse from D", want)
		}
	}
}

func TestTraverse_DepthLimit(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 1, 100)

	// Depth 1: A -> B, A -> E (only direct neighbors)
	names := nodeNames(result.Nodes)
	if !contains(names, "A") || !contains(names, "B") || !contains(names, "E") {
		t.Errorf("depth-1 should include A, B, E; got %v", names)
	}
	if contains(names, "C") || contains(names, "D") {
		t.Errorf("depth-1 should NOT include C or D; got %v", names)
	}
}

func TestTraverse_MaxNodesLimit(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", nil, nil, 10, 3)

	// Should only return at most 3 nodes
	if len(result.Nodes) > 3 {
		t.Errorf("maxNodes=3 but got %d nodes", len(result.Nodes))
	}
	if !result.Stats.Truncated {
		t.Error("should be truncated with maxNodes=3")
	}
}

func TestTraverse_RelationKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Only follow "calls" relations from A
	result := g.Traverse("A", "forward", []string{RelCalls}, nil, 10, 100)

	names := nodeNames(result.Nodes)
	// A --calls-> B --calls-> C --calls-> D (imports to E skipped)
	for _, want := range []string{"A", "B", "C", "D"} {
		if !contains(names, want) {
			t.Errorf("calls-only traverse missing %q", want)
		}
	}
	if contains(names, "E") {
		t.Error("E should not be reachable via calls-only")
	}
}

func TestTraverse_NodeKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Traverse from A but only include module-kind nodes in results
	// C and E are modules, A/B/D are symbols
	result := g.Traverse("A", "forward", nil, []string{KindModule}, 10, 100)

	names := nodeNames(result.Nodes)
	// Should traverse through symbols but only include modules in result
	// A(sym) -> B(sym) -> C(mod) -> D(sym), A -> E(mod) -> C
	if !contains(names, "C") || !contains(names, "E") {
		t.Errorf("module filter should include C and E; got %v", names)
	}
	// Start node A is always included regardless of filter
	if !contains(names, "A") {
		t.Errorf("start node A should always be included; got %v", names)
	}
}

func TestTraverse_CycleHandling(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.Traverse("A", "forward", nil, nil, 20, 100)

	// Should visit A, B, C without infinite loop
	if result.Stats.NodesVisited != 3 {
		t.Errorf("NodesVisited = %d, want 3 (cycle should be handled)", result.Stats.NodesVisited)
	}
	names := nodeNames(result.Nodes)
	for _, want := range []string{"A", "B", "C"} {
		if !contains(names, want) {
			t.Errorf("cycle traverse missing %q", want)
		}
	}
}

func TestTraverse_DisconnectedNode(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("F", "forward", nil, nil, 10, 100)

	// F is disconnected, should only return F itself
	if len(result.Nodes) != 1 || result.Nodes[0].Name != "F" {
		t.Errorf("disconnected traverse: got %v, want [F]", nodeNames(result.Nodes))
	}
}

func TestTraverse_NonexistentStart(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("NONEXISTENT", "forward", nil, nil, 10, 100)

	// Should still return the start node (with no metadata)
	if len(result.Nodes) != 1 || result.Nodes[0].Name != "NONEXISTENT" {
		t.Errorf("nonexistent start: got %v, want [NONEXISTENT]", nodeNames(result.Nodes))
	}
}

func TestFindPath_DirectConnection(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "B", nil, 10)

	if !result.Found {
		t.Fatal("path A->B should be found")
	}
	if len(result.Path) != 2 {
		t.Errorf("path length = %d, want 2 (A, B)", len(result.Path))
	}
	if result.Path[0].Name != "A" || result.Path[1].Name != "B" {
		t.Errorf("path = %v, want [A, B]", pathNames(result.Path))
	}
}

func TestFindPath_MultiHop(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "D", nil, 10)

	if !result.Found {
		t.Fatal("path A->D should be found")
	}
	// Shortest: A -> B -> C -> D
	if len(result.Path) != 4 {
		t.Errorf("path length = %d, want 4 (A, B, C, D); path = %v", len(result.Path), pathNames(result.Path))
	}

	// Check edges
	if len(result.Edges) != 3 {
		t.Errorf("edges = %d, want 3", len(result.Edges))
	}
}

func TestFindPath_NoPath(t *testing.T) {
	g, _ := buildTestGraph()

	// F is disconnected
	result := g.FindPath("A", "F", nil, 10)

	if result.Found {
		t.Error("path A->F should not exist")
	}
	if len(result.Path) != 0 {
		t.Errorf("path should be empty, got %v", pathNames(result.Path))
	}
}

func TestFindPath_SameNode(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "A", nil, 10)

	if !result.Found {
		t.Fatal("path A->A should be found (trivial)")
	}
	if len(result.Path) != 1 {
		t.Errorf("path length = %d, want 1 (just A)", len(result.Path))
	}
}

func TestFindPath_DepthLimit(t *testing.T) {
	g, _ := buildTestGraph()

	// A -> D needs 3 hops, limit to 2
	result := g.FindPath("A", "D", nil, 2)

	if result.Found {
		t.Error("path A->D should not be found with maxDepth=2")
	}
}

func TestFindPath_RelationKindFilter(t *testing.T) {
	g, _ := buildTestGraph()

	// Only imports: A --imports-> E, but no path from E to D via imports
	result := g.FindPath("A", "D", []string{RelImports}, 10)

	if result.Found {
		t.Error("path A->D via imports only should not exist")
	}
}

func TestFindPath_WithCycle(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.FindPath("A", "C", nil, 10)

	if !result.Found {
		t.Fatal("path A->C should be found")
	}
	// Shortest: A -> B -> C
	if len(result.Path) != 3 {
		t.Errorf("path length = %d, want 3; path = %v", len(result.Path), pathNames(result.Path))
	}
}

func TestImpactSet_Basic(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.ImpactSet("C", 10, 100, false)

	if result.Target != "C" {
		t.Errorf("Target = %q, want C", result.Target)
	}

	// Who depends on C? B calls C, E calls C, A calls B and imports E
	// Reverse: C <- B <- A, C <- E <- A (A already counted)
	// Depth 1: B, E
	// Depth 2: A (via B), A (via E, already counted)
	depth1 := result.ByDepth[1]
	depth1Names := make([]string, len(depth1))
	for i, n := range depth1 {
		depth1Names[i] = n.Name
	}
	if len(depth1) != 2 {
		t.Errorf("depth 1 = %d (%v), want 2 (B, E)", len(depth1), depth1Names)
	}
	if !contains(depth1Names, "B") || !contains(depth1Names, "E") {
		t.Errorf("depth 1 should include B and E; got %v", depth1Names)
	}

	depth2 := result.ByDepth[2]
	if len(depth2) != 1 || depth2[0].Name != "A" {
		depth2Names := make([]string, len(depth2))
		for i, n := range depth2 {
			depth2Names[i] = n.Name
		}
		t.Errorf("depth 2 = %v, want [A]", depth2Names)
	}

	if result.Summary == "" {
		t.Error("summary should not be empty")
	}
}

func TestImpactSet_WithForward(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.ImpactSet("C", 10, 100, true)

	if result.Forward == nil {
		t.Fatal("forward dependencies should be included")
	}

	// C -> D (forward)
	names := nodeNames(result.Forward.Nodes)
	if !contains(names, "D") {
		t.Errorf("forward from C should include D; got %v", names)
	}
}

func TestImpactSet_LeafNode(t *testing.T) {
	g, _ := buildTestGraph()

	// D has no dependents
	result := g.ImpactSet("D", 10, 100, false)

	totalDependents := 0
	for _, nodes := range result.ByDepth {
		totalDependents += len(nodes)
	}

	// C calls D, B calls C, E calls C, A calls B and imports E
	if totalDependents < 1 {
		t.Errorf("D should have at least C as direct dependent, got %d total", totalDependents)
	}
}

func TestImpactSet_CycleHandling(t *testing.T) {
	g, _ := buildCyclicGraph()

	result := g.ImpactSet("A", 20, 100, false)

	// In a cycle A->B->C->A, impact of A is: B (depth 1 reverse from A via C->A),
	// Actually reverse: who points TO A? C points to A. Who points to C? B. Who points to B? A (already visited)
	// So: depth 1: C, depth 2: B
	totalDependents := 0
	for _, nodes := range result.ByDepth {
		totalDependents += len(nodes)
	}
	if totalDependents != 2 {
		t.Errorf("cycle impact should have 2 dependents (B, C), got %d", totalDependents)
	}
}

func TestBuildGraph_ViaStore(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "X", File: "x.go", Relations: []Relation{
			{Kind: RelCalls, Target: "Y"},
		}},
		Fact{Kind: KindSymbol, Name: "Y", File: "y.go"},
	)

	// Before BuildGraph
	if s.Graph() != nil {
		t.Error("Graph should be nil before BuildGraph")
	}

	s.BuildGraph()

	g := s.Graph()
	if g == nil {
		t.Fatal("Graph should not be nil after BuildGraph")
	}
	if g.NodeCount() != 2 {
		t.Errorf("NodeCount = %d, want 2", g.NodeCount())
	}
	if g.EdgeCount() != 1 {
		t.Errorf("EdgeCount = %d, want 1", g.EdgeCount())
	}
}

func TestBuildGraph_ClearedByStoreClear(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindSymbol, Name: "X", File: "x.go"})
	s.BuildGraph()
	if s.Graph() == nil {
		t.Fatal("Graph should exist after BuildGraph")
	}

	s.Clear()
	if s.Graph() != nil {
		t.Error("Graph should be nil after Clear")
	}
}

func TestTraverse_DefaultParameters(t *testing.T) {
	g, _ := buildTestGraph()

	// Test with zero values (should use defaults)
	result := g.Traverse("A", "forward", nil, nil, 0, 0)

	// Default maxDepth=5, maxNodes=100
	// Should still find all reachable nodes
	names := nodeNames(result.Nodes)
	if len(names) < 5 {
		t.Errorf("default params should find all reachable nodes; got %d", len(names))
	}
}

func TestFindPath_DefaultMaxDepth(t *testing.T) {
	g, _ := buildTestGraph()

	// maxDepth=0 should use default (10)
	result := g.FindPath("A", "D", nil, 0)

	if !result.Found {
		t.Error("should find path A->D with default maxDepth")
	}
}

func TestTraverse_EdgesAreRecorded(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.Traverse("A", "forward", []string{RelCalls}, nil, 1, 100)

	// A --calls-> B only (depth 1, calls only)
	if len(result.Edges) != 1 {
		t.Errorf("edges = %d, want 1", len(result.Edges))
	}
	if len(result.Edges) > 0 {
		e := result.Edges[0]
		if e.Source != "A" || e.Target != "B" || e.Kind != RelCalls {
			t.Errorf("edge = %+v, want A->B calls", e)
		}
	}
}

func TestFindPath_EdgesHaveCorrectKinds(t *testing.T) {
	g, _ := buildTestGraph()

	result := g.FindPath("A", "C", nil, 10)

	if !result.Found {
		t.Fatal("path should be found")
	}

	// Shortest path A->B->C, edges should have their relation kinds
	for _, e := range result.Edges {
		if e.Kind == "" {
			t.Error("edge kind should not be empty")
		}
	}
}

func TestNewGraph_DeduplicatesEdges(t *testing.T) {
	s := NewStore()
	// Two facts with identical relations (same source->kind->target).
	s.Add(
		Fact{Kind: KindDependency, Name: "dep1", File: "models/a.rb", Relations: []Relation{
			{Kind: RelDependsOn, Target: "User"},
		}},
		Fact{Kind: KindDependency, Name: "dep2", File: "models/b.rb", Relations: []Relation{
			{Kind: RelDependsOn, Target: "User"},
		}},
		Fact{Kind: KindSymbol, Name: "User", File: "models/user.rb"},
		// A fact with a duplicate relation on itself.
		Fact{Kind: KindModule, Name: "A", File: "a.rb", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		// Another fact that also creates the same A->imports->B edge.
		Fact{Kind: KindDependency, Name: "A -> B", File: "a.rb", Relations: []Relation{
			{Kind: RelImports, Target: "B"},
		}},
		Fact{Kind: KindModule, Name: "B", File: "b.rb"},
	)
	s.BuildGraph()
	g := s.Graph()

	// dep1->User and dep2->User are distinct source nodes, so both edges exist.
	// But A->imports->B should appear only once despite two facts creating it.
	fwd := g.Forward()
	aEdges := fwd["A"]
	count := 0
	for _, e := range aEdges {
		if e.RelKind == RelImports && e.Target == "B" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("A->imports->B should appear exactly once, got %d", count)
	}

	// Reverse: B should have exactly one incoming imports edge from A.
	rev := g.Reverse()
	bEdges := rev["B"]
	count = 0
	for _, e := range bEdges {
		if e.RelKind == RelImports && e.Target == "A" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("B reverse imports from A should appear exactly once, got %d", count)
	}
}

// --- helpers ---

func nodeNames(nodes []TraversalNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

func pathNames(nodes []TraversalNode) []string {
	return nodeNames(nodes)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestNewGraph_CrossRepoCallNormalisation verifies that cross-repo call targets
// whose import paths include a known Go module path are normalised to the
// repo-relative fact name when building the graph.
func TestNewGraph_CrossRepoCallNormalisation(t *testing.T) {
	// Simulate two repos loaded together:
	//   - go-auth repo: root module fact with modulePath prop, plus symbol facts
	//   - golf repo: symbol with a call target using the full external import path
	facts := []Fact{
		// go-auth root module fact — carries the Go module path
		{
			Kind: KindModule,
			Name: ".",
			Repo: "go-auth",
			Props: map[string]any{
				"package":    "goauth",
				"language":   "go",
				"modulePath": "github.com/dejo1307/go-auth",
			},
		},
		// go-auth adapters module
		{Kind: KindModule, Name: "adapters", Repo: "go-auth"},
		// go-auth symbol: adapters.AuthHandler.Login
		{
			Kind: KindSymbol,
			Name: "adapters.AuthHandler.Login",
			Repo: "go-auth",
		},
		// golf symbol with an unresolved external call target
		{
			Kind: KindSymbol,
			Name: "internal/auth.LoginWrapper.Login",
			Repo: "golf",
			Relations: []Relation{
				{Kind: RelCalls, Target: "github.com/dejo1307/go-auth/adapters.AuthHandler.Login"},
			},
		},
	}

	g := NewGraph(facts)

	// The forward edge from golf's LoginWrapper.Login should point to the
	// normalised fact name "adapters.AuthHandler.Login", not the full import path.
	edges := g.forward["internal/auth.LoginWrapper.Login"]
	if len(edges) == 0 {
		t.Fatal("expected at least one forward edge from LoginWrapper.Login")
	}
	found := false
	for _, e := range edges {
		if e.RelKind == RelCalls && e.Target == "adapters.AuthHandler.Login" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected normalised call edge to adapters.AuthHandler.Login; got edges: %v", edges)
	}
}

// TestNormalizeExternalTarget verifies the helper directly.
func TestNormalizeExternalTarget(t *testing.T) {
	mods := map[string]struct{}{"github.com/dejo1307/go-auth": {}}

	cases := []struct {
		target string
		want   string
	}{
		{"github.com/dejo1307/go-auth/adapters.Handler.Login", "adapters.Handler.Login"},
		{"github.com/dejo1307/go-auth.SecurityHeaders", "..SecurityHeaders"},
		{"github.com/other/lib/pkg.Type.Method", ""}, // no matching module
		{"github.com/dejo1307/go-auth", ""},          // no separator after module path
	}

	for _, tc := range cases {
		got := normalizeExternalTarget(tc.target, mods)
		if got != tc.want {
			t.Errorf("normalizeExternalTarget(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// buildTypeMethodStore models a Go type with a method that makes a call, plus a
// dangling call into an unanalyzed package. The struct and method are separate
// sibling facts with no edge between them, mirroring the goextractor output.
func buildTypeMethodStore() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "auth.AuthHandler", File: "auth/handler.go", Line: 1,
			Props: map[string]any{"symbol_kind": SymbolStruct}},
		Fact{Kind: KindSymbol, Name: "auth.AuthHandler.Login", File: "auth/handler.go", Line: 10,
			Props: map[string]any{"symbol_kind": SymbolMethod},
			Relations: []Relation{
				{Kind: RelCalls, Target: "jwt.Sign"},
				{Kind: RelCalls, Target: "external.Unknown"}, // no backing fact
			}},
		Fact{Kind: KindSymbol, Name: "jwt.Sign", File: "jwt/jwt.go", Line: 5,
			Props: map[string]any{"symbol_kind": SymbolFunc}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestNewGraph_StructToMethodEdges(t *testing.T) {
	g, _ := buildTypeMethodStore()

	var found bool
	for _, e := range g.Forward()["auth.AuthHandler"] {
		if e.RelKind == RelHasMethod && e.Target == "auth.AuthHandler.Login" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected has_method edge auth.AuthHandler -> auth.AuthHandler.Login, got %+v", g.Forward()["auth.AuthHandler"])
	}

	// A package-level function whose owner ("jwt") is not a type must NOT get a
	// has_method edge.
	for _, e := range g.Forward()["jwt"] {
		if e.RelKind == RelHasMethod {
			t.Errorf("unexpected has_method edge from non-type owner: %+v", e)
		}
	}
}

func TestTraverse_ForwardFromStructSurfacesMethodCalls(t *testing.T) {
	g, _ := buildTypeMethodStore()

	result := g.Traverse("auth.AuthHandler", "forward", nil, nil, 5, 100)

	names := nodeNames(result.Nodes)
	for _, want := range []string{"auth.AuthHandler.Login", "jwt.Sign"} {
		if !contains(names, want) {
			t.Errorf("forward traverse from struct missing %q; got %v", want, names)
		}
	}
}

func TestTraverse_UnresolvedTargetMarked(t *testing.T) {
	g, _ := buildTypeMethodStore()

	result := g.Traverse("auth.AuthHandler.Login", "forward", nil, nil, 5, 100)

	var sawUnresolved, sawResolved bool
	for _, n := range result.Nodes {
		switch n.Name {
		case "external.Unknown":
			sawUnresolved = true
			if !n.Unresolved {
				t.Error("external.Unknown should be marked Unresolved")
			}
		case "jwt.Sign":
			sawResolved = true
			if n.Unresolved {
				t.Error("jwt.Sign is a real fact and must not be marked Unresolved")
			}
		}
	}
	if !sawUnresolved {
		t.Error("expected an unresolved node external.Unknown in the result")
	}
	if !sawResolved {
		t.Error("expected the resolved node jwt.Sign in the result")
	}
}

// buildCrossRepoTypeStore models a go-auth library type (struct + method +
// constructor) consumed by a golf caller, mirroring how the cross-repo call
// normalisation lands golf's calls onto go-auth's local symbol names.
func buildCrossRepoTypeStore() (*Graph, *Store) {
	s := NewStore()
	s.Add(
		// go-auth: the AuthHandler type, one method, and its constructor.
		Fact{Kind: KindSymbol, Name: "adapters.AuthHandler", File: "adapters/h.go", Line: 1, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolStruct}},
		Fact{Kind: KindSymbol, Name: "adapters.AuthHandler.Login", File: "adapters/h.go", Line: 10, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolMethod}},
		Fact{Kind: KindSymbol, Name: "adapters.NewAuthHandler", File: "adapters/h.go", Line: 30, Repo: "go-auth",
			Props: map[string]any{"symbol_kind": SymbolFunc}},
		// golf: a setup function that calls the constructor and a method.
		Fact{Kind: KindSymbol, Name: "pkg/auth.Setup", File: "pkg/auth/setup.go", Line: 28, Repo: "golf",
			Props: map[string]any{"symbol_kind": SymbolFunc}, Relations: []Relation{
				{Kind: RelCalls, Target: "adapters.NewAuthHandler"},
				{Kind: RelCalls, Target: "adapters.AuthHandler.Login"},
			}},
	)
	s.BuildGraph()
	return s.Graph(), s
}

func TestImpactSet_TypeRollup(t *testing.T) {
	g, _ := buildCrossRepoTypeStore()

	// Impact on the bare struct must roll up its method + constructor and surface
	// the cross-repo caller (previously this returned "no dependents").
	res := g.ImpactSet("adapters.AuthHandler", 3, 100, false)

	names := nodeNames(impactNodes(res))
	if !contains(names, "pkg/auth.Setup") {
		t.Errorf("type rollup did not surface cross-repo caller pkg/auth.Setup; got %v", names)
	}
	// Seeds (the type entity itself) must not appear as dependents.
	for _, seed := range []string{"adapters.AuthHandler", "adapters.AuthHandler.Login", "adapters.NewAuthHandler"} {
		if contains(names, seed) {
			t.Errorf("seed %q should be excluded from dependents", seed)
		}
	}
	if !reflect.DeepEqual(res.CrossRepoImpact, []string{"golf"}) {
		t.Errorf("CrossRepoImpact = %v, want [golf]", res.CrossRepoImpact)
	}
}

func TestImpactSet_CrossRepoImpactField(t *testing.T) {
	g, _ := buildCrossRepoTypeStore()

	// A plain function target (not a type) still reports cross-repo dependents and
	// per-node repo.
	res := g.ImpactSet("adapters.NewAuthHandler", 2, 100, false)

	if !reflect.DeepEqual(res.CrossRepoImpact, []string{"golf"}) {
		t.Fatalf("CrossRepoImpact = %v, want [golf]", res.CrossRepoImpact)
	}
	var sawRepo bool
	for _, nodes := range res.ByDepth {
		for _, n := range nodes {
			if n.Name == "pkg/auth.Setup" {
				sawRepo = true
				if n.Repo != "golf" {
					t.Errorf("dependent node repo = %q, want golf", n.Repo)
				}
			}
		}
	}
	if !sawRepo {
		t.Error("expected pkg/auth.Setup among dependents")
	}
}

func TestImpactSet_NonTypeUnchanged(t *testing.T) {
	// Single-repo graph (no Repo tags): rollup is inert and CrossRepoImpact stays nil.
	g, _ := buildTestGraph()
	res := g.ImpactSet("D", 5, 100, false)

	if res.CrossRepoImpact != nil {
		t.Errorf("CrossRepoImpact should be nil for same-repo graph, got %v", res.CrossRepoImpact)
	}
	// D is called by C (a module here) — at least one dependent is found, as before.
	if len(impactNodes(res)) == 0 {
		t.Error("expected at least one dependent for D")
	}
}

// impactNodes flattens an ImpactResult's depth buckets into a node slice.
func impactNodes(res ImpactResult) []TraversalNode {
	var out []TraversalNode
	for _, nodes := range res.ByDepth {
		out = append(out, nodes...)
	}
	return out
}
