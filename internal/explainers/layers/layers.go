package layers

import (
	"context"
	"fmt"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// LayerExplainer detects architectural patterns and layer violations.
type LayerExplainer struct{}

// New creates a new LayerExplainer.
func New() *LayerExplainer {
	return &LayerExplainer{}
}

func (e *LayerExplainer) Name() string {
	return "layers"
}

// layerDef defines how we detect architectural layers from module path patterns.
type layerDef struct {
	Name     string
	Patterns []string
	Level    int // Lower level = inner/domain, higher = outer/infra
}

// Predefined layer patterns for common architectures.
var (
	// Hexagonal / Clean Architecture layers.
	// Order matters: more specific patterns (application, adapter) are checked before
	// broader ones (domain) to avoid misclassification of paths like Domain/UseCases.
	hexagonalLayers = []layerDef{
		{Name: "application", Patterns: []string{"application", "usecase", "usecases"}, Level: 1},
		{Name: "port", Patterns: []string{"port", "ports", "interface", "interfaces"}, Level: 1},
		{Name: "adapter", Patterns: []string{"adapter", "adapters", "infrastructure", "infra", "gateway", "network"}, Level: 2},
		{Name: "repository", Patterns: []string{"repository", "repositories", "repo", "repos", "store", "storage", "persistence", "db", "database"}, Level: 2},
		{Name: "presentation", Patterns: []string{"presentation", "ui", "view", "views", "screen", "screens", "page", "pages"}, Level: 3},
		{Name: "handler", Patterns: []string{"handler", "handlers", "controller", "controllers", "api", "http", "grpc", "rest"}, Level: 3},
		{Name: "domain", Patterns: []string{"domain", "entity", "entities", "model", "models", "core"}, Level: 0},
	}

	// Next.js layers
	nextjsLayers = []layerDef{
		{Name: "pages", Patterns: []string{"pages", "app"}, Level: 3},
		{Name: "components", Patterns: []string{"components", "ui"}, Level: 2},
		{Name: "hooks", Patterns: []string{"hooks"}, Level: 1},
		{Name: "lib", Patterns: []string{"lib", "utils", "helpers"}, Level: 0},
		{Name: "api", Patterns: []string{"api"}, Level: 3},
		{Name: "services", Patterns: []string{"services"}, Level: 1},
		{Name: "types", Patterns: []string{"types"}, Level: 0},
	}

	// Go standard project layout
	goStdLayers = []layerDef{
		{Name: "cmd", Patterns: []string{"cmd"}, Level: 3},
		{Name: "internal", Patterns: []string{"internal"}, Level: 1},
		{Name: "pkg", Patterns: []string{"pkg"}, Level: 0},
		{Name: "api", Patterns: []string{"api"}, Level: 2},
	}
)

// archPattern represents a detected architecture pattern with its confidence.
type archPattern struct {
	Name       string
	Confidence float64
	Layers     map[string]*layerDef
	Modules    map[string]string // module -> layer name
}

// Explain analyzes the fact store and detects architectural patterns.
func (e *LayerExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	modules := store.ByKind(facts.KindModule)
	if len(modules) == 0 {
		return nil, nil
	}

	// Detect which architecture patterns match
	patterns := e.detectPatterns(modules)

	var insights []facts.Insight

	// Report detected architecture pattern
	if best := e.bestPattern(patterns); best != nil {
		evidence := make([]facts.Evidence, 0)
		for mod, layer := range best.Modules {
			evidence = append(evidence, facts.Evidence{
				Fact:   mod,
				Detail: fmt.Sprintf("module %q maps to layer %q", mod, layer),
			})
		}

		insights = append(insights, facts.Insight{
			Title:       fmt.Sprintf("Architecture pattern: %s", best.Name),
			Description: fmt.Sprintf("Detected %s architecture pattern with %.0f%% confidence. Found %d layers with %d classified modules.", best.Name, best.Confidence*100, len(best.Layers), len(best.Modules)),
			Confidence:  best.Confidence,
			Evidence:    evidence,
			Actions: []string{
				"Ensure new code follows the detected layer structure",
				"Review cross-layer dependencies for violations",
			},
		})

		// Detect layer violations
		violations := e.detectViolations(store, best)
		insights = append(insights, violations...)
	}

	return insights, nil
}

func (e *LayerExplainer) detectPatterns(modules []facts.Fact) []*archPattern {
	var patterns []*archPattern

	for _, def := range []struct {
		name   string
		layers []layerDef
	}{
		{"hexagonal", hexagonalLayers},
		{"nextjs", nextjsLayers},
		{"go-standard", goStdLayers},
	} {
		pattern := &archPattern{
			Name:    def.name,
			Layers:  make(map[string]*layerDef),
			Modules: make(map[string]string),
		}

		matchCount := 0
		for _, mod := range modules {
			for i, layer := range def.layers {
				if matchesLayer(mod.Name, layer.Patterns) {
					pattern.Layers[layer.Name] = &def.layers[i]
					pattern.Modules[mod.Name] = layer.Name
					matchCount++
					break
				}
			}
		}

		if matchCount > 0 && len(modules) > 0 {
			// Confidence based on how many modules are classified
			coverage := float64(matchCount) / float64(len(modules))
			// Also factor in how many distinct layers are matched
			layerCoverage := float64(len(pattern.Layers)) / float64(len(def.layers))

			pattern.Confidence = (coverage*0.6 + layerCoverage*0.4)
			if pattern.Confidence > 1.0 {
				pattern.Confidence = 1.0
			}

			// Minimum threshold
			if pattern.Confidence >= 0.2 && len(pattern.Layers) >= 2 {
				patterns = append(patterns, pattern)
			}
		}
	}

	return patterns
}

func (e *LayerExplainer) bestPattern(patterns []*archPattern) *archPattern {
	if len(patterns) == 0 {
		return nil
	}

	best := patterns[0]
	for _, p := range patterns[1:] {
		if p.Confidence > best.Confidence {
			best = p
		}
	}
	return best
}

// detectViolations checks for layer boundary violations (inner layer importing outer layer).
func (e *LayerExplainer) detectViolations(store *facts.Store, pattern *archPattern) []facts.Insight {
	var insights []facts.Insight

	deps := store.ByKind(facts.KindDependency)
	for _, dep := range deps {
		sourceModule := fileDir(dep.File)
		sourceLayer, sourceOK := pattern.Modules[sourceModule]
		if !sourceOK {
			continue
		}

		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}

			targetLayer, targetOK := pattern.Modules[rel.Target]
			if !targetOK {
				continue
			}

			// Check if source layer level is lower than target
			sourceDef := pattern.Layers[sourceLayer]
			targetDef := pattern.Layers[targetLayer]

			if sourceDef != nil && targetDef != nil && sourceDef.Level < targetDef.Level {
				insights = append(insights, facts.Insight{
					Title: fmt.Sprintf("Layer violation: %s -> %s", sourceLayer, targetLayer),
					Description: fmt.Sprintf(
						"Module %q (layer: %s, level %d) imports module %q (layer: %s, level %d). "+
							"Inner layers should not depend on outer layers.",
						sourceModule, sourceLayer, sourceDef.Level,
						rel.Target, targetLayer, targetDef.Level,
					),
					Confidence: 0.8,
					Evidence: []facts.Evidence{
						{File: dep.File, Detail: fmt.Sprintf("import of %s", rel.Target)},
					},
					Actions: []string{
						"Introduce an interface/port in the inner layer",
						"Move shared types to a common package",
						"Invert the dependency using dependency injection",
					},
				})
			}
		}
	}

	return insights
}

// matchesLayer checks if a module path contains any of the given patterns.
func matchesLayer(modulePath string, patterns []string) bool {
	parts := strings.Split(strings.ToLower(modulePath), "/")
	for _, part := range parts {
		for _, pattern := range patterns {
			if part == pattern {
				return true
			}
		}
	}
	return false
}

func fileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}
