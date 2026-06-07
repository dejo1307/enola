package cycles

import (
	"context"
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// CycleExplainer detects cyclic dependencies between modules using Tarjan's SCC algorithm.
type CycleExplainer struct{}

// New creates a new CycleExplainer.
func New() *CycleExplainer {
	return &CycleExplainer{}
}

func (e *CycleExplainer) Name() string {
	return "cycles"
}

// Explain builds a dependency graph from import relations and detects cycles.
func (e *CycleExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	// Build adjacency list from dependency facts
	graph := buildDependencyGraph(store)

	// Run Tarjan's SCC
	sccs := tarjanSCC(graph)

	// Filter to cycles (SCCs with size > 1)
	var insights []facts.Insight
	for _, scc := range sccs {
		if len(scc) <= 1 {
			continue
		}

		cyclePath := strings.Join(scc, " -> ") + " -> " + scc[0]
		evidence := make([]facts.Evidence, 0, len(scc))
		for _, mod := range scc {
			evidence = append(evidence, facts.Evidence{
				Fact:   mod,
				Detail: fmt.Sprintf("module %q is part of the cycle", mod),
			})
		}

		insights = append(insights, facts.Insight{
			Title:       fmt.Sprintf("Cyclic dependency detected (%d modules)", len(scc)),
			Description: fmt.Sprintf("The following modules form a dependency cycle: %s. This can cause initialization issues, make refactoring harder, and indicates tight coupling.", cyclePath),
			Confidence:  1.0, // Deterministic
			Evidence:    evidence,
			Actions: []string{
				"Introduce an interface to break the cycle",
				"Extract shared types to a separate package",
				"Consider merging tightly coupled modules",
			},
		})
	}

	return insights, nil
}

// buildDependencyGraph extracts module-level import relationships.
func buildDependencyGraph(store *facts.Store) map[string][]string {
	graph := make(map[string][]string)

	// Get all modules
	modules := store.ByKind(facts.KindModule)
	moduleNames := make(map[string]bool)
	for _, m := range modules {
		moduleNames[m.Name] = true
		if _, ok := graph[m.Name]; !ok {
			graph[m.Name] = nil
		}
	}

	// Get all dependency/import facts
	deps := store.ByKind(facts.KindDependency)
	for _, dep := range deps {
		// Source module is the directory of the file
		sourceModule := fileDir(dep.File)

		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			target := rel.Target

			// Only track internal dependencies (module-to-module within the repo)
			// Skip external packages (those with dots like "fmt", "github.com/...", "@types/...")
			if isExternalImport(target) {
				continue
			}

			// Normalize relative imports to module paths
			if strings.HasPrefix(target, ".") {
				target = resolveRelativeImport(sourceModule, target)
			}

			if moduleNames[target] {
				graph[sourceModule] = append(graph[sourceModule], target)
			}
		}
	}

	return graph
}

func fileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

func isExternalImport(path string) bool {
	// Go external imports contain dots (fmt, net/http, github.com/...)
	// TS external imports don't start with . or / and aren't relative
	if strings.HasPrefix(path, ".") || strings.HasPrefix(path, "/") {
		return false
	}
	// Go standard library or third-party
	if strings.Contains(path, ".") || !strings.Contains(path, "/") {
		// Likely a Go stdlib or npm package (e.g., "fmt", "react", "@types/node")
		return true
	}
	return false
}

func resolveRelativeImport(sourceModule, target string) string {
	if !strings.HasPrefix(target, ".") {
		return target
	}

	parts := strings.Split(sourceModule, "/")
	targetParts := strings.Split(target, "/")

	for _, tp := range targetParts {
		switch tp {
		case ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, tp)
		}
	}

	return strings.Join(parts, "/")
}

// tarjanSCC implements Tarjan's strongly connected components algorithm.
func tarjanSCC(graph map[string][]string) [][]string {
	var (
		index    int
		stack    []string
		onStack  = make(map[string]bool)
		indices  = make(map[string]int)
		lowlinks = make(map[string]int)
		sccs     [][]string
	)

	var strongConnect func(v string)
	strongConnect = func(v string) {
		indices[v] = index
		lowlinks[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range graph[v] {
			if _, visited := indices[w]; !visited {
				strongConnect(w)
				if lowlinks[w] < lowlinks[v] {
					lowlinks[v] = lowlinks[w]
				}
			} else if onStack[w] {
				if indices[w] < lowlinks[v] {
					lowlinks[v] = indices[w]
				}
			}
		}

		// Root of an SCC
		if lowlinks[v] == indices[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for v := range graph {
		if _, visited := indices[v]; !visited {
			strongConnect(v)
		}
	}

	return sccs
}
