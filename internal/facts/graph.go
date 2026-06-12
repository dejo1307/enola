package facts

import (
	"strings"
	"sync"
)

// Graph provides adjacency-list indexes and traversal operations over a Store.
// It is a derived index rebuilt from the Store's facts after each snapshot generation.
type Graph struct {
	mu       sync.RWMutex
	forward  map[string][]Edge   // fact name → outgoing edges
	reverse  map[string][]Edge   // fact name → incoming edges
	facts    []Fact              // reference to the store's facts (for metadata lookups)
	factIdx  map[string]int      // fact name → first index in facts slice
	edgeSeen map[string]struct{} // deduplication: "source\x00kind\x00target"
}

// Edge represents a directed relationship between two facts.
type Edge struct {
	RelKind string // "imports", "calls", "declares", "implements", "depends_on", "has_method"
	Target  string // target fact name (forward) or source fact name (reverse)
}

// TraversalResult holds the output of a graph traversal.
type TraversalResult struct {
	Nodes []TraversalNode `json:"nodes"`
	Edges []TraversalEdge `json:"edges"`
	Stats TraversalStats  `json:"stats"`
}

// TraversalNode is a node visited during traversal.
type TraversalNode struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Depth int    `json:"depth"`
	// Unresolved marks a node whose name is the target of an edge but has no
	// backing fact in the store. This happens for inferred call targets that
	// could not be matched to a declared symbol (e.g. interface-method dispatch,
	// or calls into packages that weren't analyzed). The edge is real; the
	// destination symbol just isn't in the graph.
	Unresolved bool `json:"unresolved,omitempty"`
}

// TraversalEdge is an edge traversed during traversal.
type TraversalEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
}

// TraversalStats summarizes a traversal.
type TraversalStats struct {
	NodesVisited    int  `json:"nodes_visited"`
	EdgesTraversed  int  `json:"edges_traversed"`
	MaxDepthReached int  `json:"max_depth_reached"`
	Truncated       bool `json:"truncated"`
}

// ImpactResult holds depth-bucketed impact analysis results.
type ImpactResult struct {
	Target  string                  `json:"target"`
	ByDepth map[int][]TraversalNode `json:"by_depth"`
	Edges   []TraversalEdge         `json:"edges"`
	Summary string                  `json:"summary"`
	Stats   TraversalStats          `json:"stats"`
	Forward *TraversalResult        `json:"forward_dependencies,omitempty"`
}

// PathResult holds a shortest-path result.
type PathResult struct {
	From  string          `json:"from"`
	To    string          `json:"to"`
	Found bool            `json:"found"`
	Path  []TraversalNode `json:"path,omitempty"`
	Edges []TraversalEdge `json:"edges,omitempty"`
}

// NewGraph builds a Graph from a slice of facts. The graph constructs both forward
// and reverse adjacency lists in a single O(F+R) pass.
//
// For dependency facts (kind="dependency") with "imports" relations, the graph also
// creates synthetic edges from the containing module (derived from the file's directory)
// to the import target. This bridges the structural gap where modules and their
// dependencies are separate facts: module "internal/server" ←→ dependency fact
// "internal/server -> internal/config" → target "internal/config".
//
// For cross-repo call targets (e.g. "github.com/dejo1307/go-auth/adapters.Handler.Login"
// emitted by an external consumer), the graph normalises the target by stripping known
// Go module path prefixes (stored in KindModule facts as props["modulePath"]). This
// allows edges to land on the correct fact in the loaded external repo.
func NewGraph(ff []Fact) *Graph {
	g := &Graph{
		forward:  make(map[string][]Edge),
		reverse:  make(map[string][]Edge),
		facts:    ff,
		factIdx:  make(map[string]int, len(ff)),
		edgeSeen: make(map[string]struct{}),
	}

	// First pass: index all fact names, collect module names and Go module paths.
	moduleNames := make(map[string]bool)
	modulePaths := make(map[string]struct{}) // Go module paths for cross-repo normalisation
	for i, f := range ff {
		if f.Name != "" {
			if _, exists := g.factIdx[f.Name]; !exists {
				g.factIdx[f.Name] = i
			}
		}
		if f.Kind == KindModule {
			moduleNames[f.Name] = true
			if mp, ok := f.Props["modulePath"].(string); ok && mp != "" {
				modulePaths[mp] = struct{}{}
			}
		}
	}

	// Second pass: build adjacency lists
	for _, f := range ff {
		for _, rel := range f.Relations {
			target := rel.Target
			// For unresolved call targets, attempt cross-repo normalisation by
			// stripping known Go module path prefixes.
			if rel.Kind == RelCalls {
				if _, exists := g.factIdx[target]; !exists {
					if normalized := normalizeExternalTarget(target, modulePaths); normalized != "" {
						if _, exists := g.factIdx[normalized]; exists {
							target = normalized
						}
					}
				}
			}
			g.addEdge(f.Name, rel.Kind, target)
		}

		// For dependency facts with imports, also create module→target edges
		// so that traversing from a module follows through to its imports.
		// The target is resolved to the nearest ancestor that is a known module,
		// handling cases where import paths point to files within a module directory
		// (e.g., "src/types/tournament" resolves to module "src/types").
		if f.Kind == KindDependency && f.File != "" {
			modName := fileDirectory(f.File)
			if moduleNames[modName] {
				for _, rel := range f.Relations {
					if rel.Kind == RelImports {
						target := resolveToModule(rel.Target, moduleNames)
						if target != "" && target != modName {
							g.addEdge(modName, RelImports, target)
						}
					}
				}
			}
		}
	}

	// Third pass: synthesize "has_method" edges linking an owner type symbol
	// (struct/interface/class/type) to its method symbols. Extractors emit a
	// method as a sibling fact named "<owner>.<method>" with no edge back to the
	// owner, so forward traversal from a type would otherwise surface none of its
	// methods (and transitively none of their calls). This is language-agnostic:
	// any fact named "<knownType>.<member>" gets wired to its owner.
	for _, f := range ff {
		if f.Kind != KindSymbol {
			continue
		}
		sk, _ := f.Props["symbol_kind"].(string)
		if sk != SymbolMethod && sk != SymbolFunc {
			continue
		}
		if owner := g.methodOwner(f.Name); owner != "" {
			g.addEdge(owner, RelHasMethod, f.Name)
		}
	}

	// edgeSeen is only needed during construction; release it so the GC can
	// reclaim the O(edges × 3 strings) backing memory.
	g.edgeSeen = nil

	return g
}

// methodOwner returns the owner type name for a method fact name of the form
// "<owner>.<method>", but only when <owner> is itself a known symbol fact whose
// symbol_kind is a type (struct/interface/class/type). Returns "" otherwise.
func (g *Graph) methodOwner(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return ""
	}
	owner := name[:dot]
	idx, ok := g.factIdx[owner]
	if !ok || idx >= len(g.facts) {
		return ""
	}
	of := g.facts[idx]
	if of.Kind != KindSymbol {
		return ""
	}
	switch sk, _ := of.Props["symbol_kind"].(string); sk {
	case SymbolStruct, SymbolInterface, SymbolClass, SymbolType:
		return owner
	}
	return ""
}

// Traverse performs a BFS traversal from the given start node.
// direction is "forward" or "reverse".
// relKinds filters to specific relation types (nil = all).
// nodeKinds filters result nodes to specific fact kinds (nil = all).
// maxDepth limits traversal depth (0 = use default 5).
// maxNodes limits total returned nodes (0 = use default 100).
func (g *Graph) Traverse(start, direction string, relKinds, nodeKinds []string, maxDepth, maxNodes int) TraversalResult {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 5
	}
	if maxDepth > 20 {
		maxDepth = 20
	}
	if maxNodes <= 0 {
		maxNodes = 100
	}
	if maxNodes > 500 {
		maxNodes = 500
	}

	adj := g.forward
	if direction == "reverse" {
		adj = g.reverse
	}

	relSet := toSet(relKinds)
	kindSet := toSet(nodeKinds)

	var result TraversalResult
	visited := make(map[string]bool)

	type queueItem struct {
		name  string
		depth int
	}

	// Add start node
	visited[start] = true
	queue := []queueItem{{name: start, depth: 0}}
	result.Nodes = append(result.Nodes, g.nodeFor(start, 0))

	truncated := false
	maxDepthReached := 0

	// Use an index pointer instead of re-slicing to avoid keeping the full
	// backing array alive for the duration of traversal.
	for qi := 0; qi < len(queue); qi++ {
		item := queue[qi]

		if item.depth >= maxDepth {
			continue
		}

		edges := adj[item.name]
		for _, e := range edges {
			if relSet != nil {
				if _, ok := relSet[e.RelKind]; !ok {
					continue
				}
			}

			result.Stats.EdgesTraversed++

			// Record the edge
			if direction == "reverse" {
				result.Edges = append(result.Edges, TraversalEdge{
					Source: e.Target,
					Target: item.name,
					Kind:   e.RelKind,
				})
			} else {
				result.Edges = append(result.Edges, TraversalEdge{
					Source: item.name,
					Target: e.Target,
					Kind:   e.RelKind,
				})
			}

			if visited[e.Target] {
				continue
			}
			visited[e.Target] = true

			newDepth := item.depth + 1
			if newDepth > maxDepthReached {
				maxDepthReached = newDepth
			}

			node := g.nodeFor(e.Target, newDepth)

			// Apply node kind filter
			if kindSet != nil {
				if _, ok := kindSet[node.Kind]; !ok {
					// Still traverse through this node but don't include it in results
					queue = append(queue, queueItem{name: e.Target, depth: newDepth})
					continue
				}
			}

			if len(result.Nodes) >= maxNodes {
				truncated = true
				continue
			}

			result.Nodes = append(result.Nodes, node)
			queue = append(queue, queueItem{name: e.Target, depth: newDepth})
		}
	}

	result.Stats.NodesVisited = len(visited)
	result.Stats.MaxDepthReached = maxDepthReached
	result.Stats.Truncated = truncated

	return result
}

// FindPath finds the shortest path between two nodes using BFS.
// relKinds filters to specific relation types (nil = all).
// maxDepth limits search depth (0 = use default 10).
func (g *Graph) FindPath(from, to string, relKinds []string, maxDepth int) PathResult {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 20 {
		maxDepth = 20
	}

	// Trivial case: same node
	if from == to {
		return PathResult{
			From:  from,
			To:    to,
			Found: true,
			Path:  []TraversalNode{g.nodeFor(from, 0)},
		}
	}

	relSet := toSet(relKinds)

	type queueItem struct {
		name  string
		depth int
	}

	visited := make(map[string]bool)
	parent := make(map[string]string)   // child → parent
	parentEdge := make(map[string]Edge) // child → edge from parent

	visited[from] = true
	queue := []queueItem{{name: from, depth: 0}}

	found := false
	// Use an index pointer to avoid keeping the full backing array alive.
	for qi := 0; qi < len(queue) && !found; qi++ {
		item := queue[qi]

		if item.depth >= maxDepth {
			continue
		}

		for _, e := range g.forward[item.name] {
			if relSet != nil {
				if _, ok := relSet[e.RelKind]; !ok {
					continue
				}
			}
			if visited[e.Target] {
				continue
			}
			visited[e.Target] = true
			parent[e.Target] = item.name
			parentEdge[e.Target] = e

			if e.Target == to {
				found = true
				break
			}
			queue = append(queue, queueItem{name: e.Target, depth: item.depth + 1})
		}
	}

	result := PathResult{From: from, To: to, Found: found}
	if !found {
		return result
	}

	// Reconstruct path
	var path []string
	for cur := to; cur != from; cur = parent[cur] {
		path = append(path, cur)
	}
	path = append(path, from)

	// Reverse to get from → to order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	for i, name := range path {
		result.Path = append(result.Path, g.nodeFor(name, i))
	}

	// Reconstruct edges along the path
	for i := 1; i < len(path); i++ {
		e := parentEdge[path[i]]
		result.Edges = append(result.Edges, TraversalEdge{
			Source: path[i-1],
			Target: path[i],
			Kind:   e.RelKind,
		})
	}

	return result
}

// ImpactSet computes the transitive set of nodes affected by changing the target.
// It performs a reverse BFS and groups results by depth.
// If includeForward is true, it also includes what the target depends on.
func (g *Graph) ImpactSet(target string, maxDepth, maxNodes int, includeForward bool) ImpactResult {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 10 {
		maxDepth = 10
	}
	if maxNodes <= 0 {
		maxNodes = 200
	}
	if maxNodes > 500 {
		maxNodes = 500
	}

	// Reverse traversal: who depends on target?
	rev := g.Traverse(target, "reverse", nil, nil, maxDepth, maxNodes)

	result := ImpactResult{
		Target:  target,
		ByDepth: make(map[int][]TraversalNode),
		Edges:   rev.Edges,
		Stats:   rev.Stats,
	}

	// Bucket nodes by depth (skip depth 0 which is the target itself)
	for _, n := range rev.Nodes {
		if n.Depth > 0 {
			result.ByDepth[n.Depth] = append(result.ByDepth[n.Depth], n)
		}
	}

	// Build summary
	result.Summary = g.buildImpactSummary(result.ByDepth)

	// Optionally include forward dependencies
	if includeForward {
		fwd := g.Traverse(target, "forward", nil, nil, maxDepth, maxNodes)
		result.Forward = &fwd
	}

	return result
}

// normalizeExternalTarget strips a known Go module path prefix from a call
// target that doesn't match any fact. This bridges cross-repo call edges where
// the consumer emits the full import path (e.g. "github.com/x/go-auth/adapters.Handler.Login")
// but the provider's facts use the repo-relative path (e.g. "adapters.Handler.Login").
//
//	subpackage: "github.com/x/go-auth/adapters.Handler.Login" → "adapters.Handler.Login"
//	root pkg:   "github.com/x/go-auth.SecurityHeaders"        → "..SecurityHeaders"
func normalizeExternalTarget(target string, modulePaths map[string]struct{}) string {
	for modulePath := range modulePaths {
		if !strings.HasPrefix(target, modulePath) {
			continue
		}
		after := target[len(modulePath):]
		switch {
		case strings.HasPrefix(after, "/"):
			return after[1:] // subpackage: strip leading "/"
		case strings.HasPrefix(after, "."):
			return "." + after // root pkg: ".Sym" → "..Sym" (pkgDir="." naming)
		}
	}
	return ""
}

func (g *Graph) addEdge(source, relKind, target string) {
	key := source + "\x00" + relKind + "\x00" + target
	if _, exists := g.edgeSeen[key]; exists {
		return
	}
	g.edgeSeen[key] = struct{}{}
	g.forward[source] = append(g.forward[source], Edge{
		RelKind: relKind,
		Target:  target,
	})
	g.reverse[target] = append(g.reverse[target], Edge{
		RelKind: relKind,
		Target:  source,
	})
}

// resolveToModule finds the closest matching module for a target by trying
// the target itself, then walking up parent directories until a match is found.
func resolveToModule(target string, moduleNames map[string]bool) string {
	cur := target
	for {
		if moduleNames[cur] {
			return cur
		}
		parent := fileDirectory(cur)
		if parent == cur || parent == "." {
			break
		}
		cur = parent
	}
	return ""
}

func fileDirectory(file string) string {
	if i := strings.LastIndex(file, "/"); i >= 0 {
		return file[:i]
	}
	return "."
}

// ReverseFacts returns all facts that have a relation targeting targetName.
// When relKind is non-empty only edges of that kind are considered.
// It uses the reverse adjacency index (O(1) lookup) instead of scanning all facts.
func (g *Graph) ReverseFacts(targetName, relKind string) []Fact {
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := g.reverse[targetName]
	if len(edges) == 0 {
		return nil
	}

	result := make([]Fact, 0, len(edges))
	seen := make(map[string]struct{}, len(edges))
	for _, e := range edges {
		if relKind != "" && e.RelKind != relKind {
			continue
		}
		sourceName := e.Target // reverse edge stores the source in Target field
		if _, already := seen[sourceName]; already {
			continue
		}
		seen[sourceName] = struct{}{}
		if idx, ok := g.factIdx[sourceName]; ok && idx < len(g.facts) {
			result = append(result, g.facts[idx])
		}
	}
	return result
}

// Forward returns the forward adjacency map (for use by explainers like cycles).
func (g *Graph) Forward() map[string][]Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.forward
}

// Reverse returns the reverse adjacency map.
func (g *Graph) Reverse() map[string][]Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.reverse
}

// NodeCount returns the number of unique nodes in the graph.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.factIdx)
}

// EdgeCount returns the total number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	count := 0
	for _, edges := range g.forward {
		count += len(edges)
	}
	return count
}

func (g *Graph) nodeFor(name string, depth int) TraversalNode {
	node := TraversalNode{Name: name, Depth: depth}
	if idx, ok := g.factIdx[name]; ok && idx < len(g.facts) {
		f := g.facts[idx]
		node.Kind = f.Kind
		node.File = f.File
		node.Line = f.Line
	} else {
		// No backing fact: this is a dangling edge target (e.g. an inferred call
		// into an unanalyzed package or an interface method). Mark it honestly
		// rather than emitting a silent kind-less node.
		node.Unresolved = true
	}
	return node
}

func (g *Graph) buildImpactSummary(byDepth map[int][]TraversalNode) string {
	if len(byDepth) == 0 {
		return "No dependents found."
	}

	total := 0
	for _, nodes := range byDepth {
		total += len(nodes)
	}

	summary := ""
	for d := 1; d <= 10; d++ {
		nodes := byDepth[d]
		if len(nodes) == 0 {
			continue
		}
		// Count by kind
		kindCount := make(map[string]int)
		for _, n := range nodes {
			k := n.Kind
			if k == "" {
				k = "unknown"
			}
			kindCount[k]++
		}
		if summary != "" {
			summary += "; "
		}
		summary += "depth " + itoa(d) + ": "
		first := true
		for kind, count := range kindCount {
			if !first {
				summary += ", "
			}
			summary += itoa(count) + " " + kind
			if count > 1 {
				summary += "s"
			}
			first = false
		}
	}

	return itoa(total) + " total dependents — " + summary
}

func toSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
