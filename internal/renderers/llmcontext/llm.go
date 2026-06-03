package llmcontext

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// LLMContextRenderer produces a compact markdown summary optimized for LLM consumption.
type LLMContextRenderer struct {
	maxTokens int
}

// New creates a new LLMContextRenderer with the given token budget.
func New(maxTokens int) *LLMContextRenderer {
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	return &LLMContextRenderer{maxTokens: maxTokens}
}

func (r *LLMContextRenderer) Name() string {
	return "llm_context"
}

// section holds a rendered section with its display name.
type section struct {
	name    string
	content string
}

// Render produces the llm_context.md artifact using progressive summarization.
// Sections are ordered by priority; lower-priority sections are omitted first
// when the token budget is tight.
func (r *LLMContextRenderer) Render(ctx context.Context, snapshot *facts.Snapshot) ([]facts.Artifact, error) {
	// Sections ordered by priority (most important first)
	sections := []section{
		{"Repository Map", r.renderRepoMap(snapshot)},
		{"Architecture Pattern", r.renderArchPattern(snapshot)},
		{"Cross-Repo Dependencies", r.renderCrossRepo(snapshot)},
		{"Entry Points", r.renderEntryPoints(snapshot)},
		{"Routes", r.renderRoutes(snapshot)},
		{"Storage", r.renderStorage(snapshot)},
		{"Dependency Rules", r.renderDependencyRules(snapshot)},
		{"Critical Modules", r.renderCriticalModules(snapshot)},
		{"Risk Zones", r.renderRiskZones(snapshot)},
		{"How to Add a Feature", r.renderFeatureGuide(snapshot)},
		{"Meta", r.renderMeta(snapshot)},
	}

	header := "# Architecture Snapshot\n\n"
	maxChars := r.maxTokens * 4 // rough estimate: 1 token ~= 4 chars
	remaining := maxChars - len(header)

	var sb strings.Builder
	sb.WriteString(header)

	for i, sec := range sections {
		if sec.content == "" {
			continue
		}
		if len(sec.content) <= remaining {
			sb.WriteString(sec.content)
			remaining -= len(sec.content)
		} else if remaining > 200 {
			// Partially include this section
			cutpoint := remaining - 100
			if cutpoint < 0 {
				cutpoint = 0
			}
			sb.WriteString(sec.content[:cutpoint])
			sb.WriteString(fmt.Sprintf("\n\n---\n*[Truncated in: %s]*\n", sec.name))
			remaining = 0
			break
		} else {
			// List omitted sections
			var omitted []string
			for _, s := range sections[i:] {
				if s.content != "" {
					omitted = append(omitted, s.name)
				}
			}
			sb.WriteString(fmt.Sprintf("\n\n---\n*[Omitted: %s]*\n", strings.Join(omitted, ", ")))
			break
		}
	}

	return []facts.Artifact{
		{
			Name:    "llm_context.md",
			Content: []byte(sb.String()),
			Type:    "text/markdown",
		},
	}, nil
}

func (r *LLMContextRenderer) renderRepoMap(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Repository Map\n\n")

	modules := filterByKind(snapshot.Facts, facts.KindModule)
	if len(modules) == 0 {
		sb.WriteString("_No modules detected._\n\n")
		return sb.String()
	}

	// Group symbols by module
	symbolCounts := make(map[string]int)
	exportedCounts := make(map[string]int)
	for _, f := range snapshot.Facts {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDeclares {
				symbolCounts[rel.Target]++
				if exported, ok := f.Props["exported"].(bool); ok && exported {
					exportedCounts[rel.Target]++
				}
			}
		}
	}

	// Sort modules by name
	sort.Slice(modules, func(i, j int) bool {
		return modules[i].Name < modules[j].Name
	})

	sb.WriteString("| Module | Language | Symbols | Exported |\n")
	sb.WriteString("|--------|----------|---------|----------|\n")
	for _, mod := range modules {
		lang := "unknown"
		if l, ok := mod.Props["language"].(string); ok {
			lang = l
		}
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %d | %d |\n",
			mod.Name, lang, symbolCounts[mod.Name], exportedCounts[mod.Name]))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderArchPattern(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Architecture Pattern\n\n")

	// Find architecture insights
	for _, insight := range snapshot.Insights {
		if strings.HasPrefix(insight.Title, "Architecture pattern:") {
			sb.WriteString(fmt.Sprintf("**%s** (confidence: %.0f%%)\n\n", insight.Title, insight.Confidence*100))
			sb.WriteString(insight.Description + "\n\n")

			if len(insight.Evidence) > 0 {
				sb.WriteString("Layer mapping:\n")
				for _, ev := range insight.Evidence {
					sb.WriteString(fmt.Sprintf("- %s\n", ev.Detail))
				}
				sb.WriteString("\n")
			}
			return sb.String()
		}
	}

	sb.WriteString("_No specific architecture pattern detected._\n\n")
	return sb.String()
}

// renderCrossRepo surfaces the cross-repo "graph of graphs": one row per
// consumer→provider dependency synthesized by the crossrepo linker. It renders
// regardless of which explainers are enabled, so multi-repo dependencies are
// always visible in the context an agent reads. Returns "" when there are none
// (i.e. single-repo snapshots).
func (r *LLMContextRenderer) renderCrossRepo(snapshot *facts.Snapshot) string {
	var edges []facts.Fact
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindDependency && propStr(f, "type") == "cross_repo" {
			edges = append(edges, f)
		}
	}
	if len(edges) == 0 {
		return ""
	}

	sort.Slice(edges, func(i, j int) bool { return edges[i].Name < edges[j].Name })

	var sb strings.Builder
	sb.WriteString("## Cross-Repo Dependencies\n\n")
	sb.WriteString("How requests and code flow between repositories. Traverse from a repo label " +
		"(service node) to follow these edges.\n\n")
	sb.WriteString("| Consumer | Provider | Via | Detail |\n")
	sb.WriteString("|----------|----------|-----|--------|\n")
	for _, e := range edges {
		consumer := e.Repo
		provider := consumer
		for _, rel := range e.Relations {
			if rel.Kind == facts.RelDependsOn {
				provider = rel.Target
			}
		}
		// The provider also lives in the edge name ("consumer -> provider") for
		// facts loaded from JSONL where relations may be absent.
		if provider == consumer {
			if i := strings.Index(e.Name, " -> "); i >= 0 {
				provider = e.Name[i+4:]
			}
		}
		via := strings.Join(propStrSlice(e, "via"), "+")
		sb.WriteString(fmt.Sprintf("| `%s` | `%s` | %s | %s |\n", consumer, provider, via, crossRepoDetail(e)))
	}
	sb.WriteString("\n")
	return sb.String()
}

// crossRepoDetail renders the evidence behind an edge: endpoint/import counts
// plus a few samples.
func crossRepoDetail(e facts.Fact) string {
	var parts []string
	if n := propInt(e, "endpoint_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d endpoint(s): %s", n, samplePreview(propStrSlice(e, "endpoints"))))
	}
	if n := propInt(e, "import_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d import(s): %s", n, samplePreview(propStrSlice(e, "import_samples"))))
	}
	if len(parts) == 0 {
		return "cross-repo dependency"
	}
	return strings.Join(parts, "; ")
}

// samplePreview joins up to three samples, appending "…" when truncated.
func samplePreview(ss []string) string {
	if len(ss) <= 3 {
		return strings.Join(ss, ", ")
	}
	return strings.Join(ss[:3], ", ") + ", …"
}

func (r *LLMContextRenderer) renderEntryPoints(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Entry Points\n\n")

	var entryPoints []string

	for _, f := range snapshot.Facts {
		if f.Kind != facts.KindSymbol {
			continue
		}

		symbolKind, _ := f.Props["symbol_kind"].(string)
		exported, _ := f.Props["exported"].(bool)

		// Main functions
		if strings.HasSuffix(f.Name, ".main") && symbolKind == facts.SymbolFunc {
			entryPoints = append(entryPoints, fmt.Sprintf("- **main**: `%s` (%s)", f.Name, f.File))
		}

		// HTTP handlers (common patterns)
		if exported && symbolKind == facts.SymbolFunc {
			nameLower := strings.ToLower(f.Name)
			if strings.Contains(nameLower, "handler") || strings.Contains(nameLower, "handle") ||
				strings.Contains(nameLower, "serve") {
				entryPoints = append(entryPoints, fmt.Sprintf("- **handler**: `%s` (%s)", f.Name, f.File))
			}
		}

		// iOS/macOS app entry point (@main struct conforming to App)
		if iosComp, _ := f.Props["ios_component"].(string); iosComp == "swiftui_app" {
			entryPoints = append(entryPoints, fmt.Sprintf("- **app**: `%s` (%s)", f.Name, f.File))
		}
	}

	// Routes as entry points
	routes := filterByKind(snapshot.Facts, facts.KindRoute)
	for _, route := range routes {
		method, _ := route.Props["method"].(string)
		entryPoints = append(entryPoints, fmt.Sprintf("- **route** %s `%s` (%s)", method, route.Name, route.File))
	}

	if len(entryPoints) == 0 {
		sb.WriteString("_No entry points detected._\n\n")
		return sb.String()
	}

	sort.Strings(entryPoints)
	for _, ep := range entryPoints {
		sb.WriteString(ep + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderRoutes(snapshot *facts.Snapshot) string {
	routes := filterByKind(snapshot.Facts, facts.KindRoute)
	if len(routes) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Routes\n\n")
	sb.WriteString("| Method | Path | File | Type |\n")
	sb.WriteString("|--------|------|------|------|\n")

	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Name < routes[j].Name
	})

	for _, route := range routes {
		method, _ := route.Props["method"].(string)
		routeType, _ := route.Props["type"].(string)
		sb.WriteString(fmt.Sprintf("| %s | `%s` | `%s` | %s |\n", method, route.Name, route.File, routeType))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderStorage(snapshot *facts.Snapshot) string {
	storage := filterByKind(snapshot.Facts, facts.KindStorage)
	if len(storage) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Storage\n\n")
	sb.WriteString("| Name | Kind | Operation | File |\n")
	sb.WriteString("|------|------|-----------|------|\n")

	sort.Slice(storage, func(i, j int) bool {
		return storage[i].Name < storage[j].Name
	})

	for _, s := range storage {
		storageKind, _ := s.Props["storage_kind"].(string)
		operation, _ := s.Props["operation"].(string)
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %s | `%s` |\n",
			s.Name, storageKind, operation, s.File))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderDependencyRules(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Dependency Rules\n\n")

	// Collect unique module-to-module internal dependencies
	type depEdge struct{ from, to string }
	seen := make(map[depEdge]bool)

	deps := filterByKind(snapshot.Facts, facts.KindDependency)
	modules := make(map[string]bool)
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindModule {
			modules[f.Name] = true
		}
	}

	var edges []string
	for _, dep := range deps {
		sourceModule := fileDir(dep.File)
		for _, rel := range dep.Relations {
			if rel.Kind != facts.RelImports {
				continue
			}
			if !modules[rel.Target] {
				continue
			}
			edge := depEdge{sourceModule, rel.Target}
			if !seen[edge] {
				seen[edge] = true
				edges = append(edges, fmt.Sprintf("- `%s` -> `%s`", edge.from, edge.to))
			}
		}
	}

	if len(edges) == 0 {
		sb.WriteString("_No internal dependency rules detected._\n\n")
		return sb.String()
	}

	sort.Strings(edges)
	for _, e := range edges {
		sb.WriteString(e + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderCriticalModules(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## Critical Modules\n\n")

	// Compute fan-in (imported by others) and fan-out (imports others)
	fanIn := make(map[string]int)
	fanOut := make(map[string]int)

	modules := make(map[string]bool)
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindModule {
			modules[f.Name] = true
		}
	}

	deps := filterByKind(snapshot.Facts, facts.KindDependency)
	for _, dep := range deps {
		sourceModule := fileDir(dep.File)
		for _, rel := range dep.Relations {
			if rel.Kind == facts.RelImports && modules[rel.Target] {
				fanOut[sourceModule]++
				fanIn[rel.Target]++
			}
		}
	}

	type modScore struct {
		Name   string
		FanIn  int
		FanOut int
		Score  int
	}

	var scored []modScore
	for mod := range modules {
		s := modScore{
			Name:   mod,
			FanIn:  fanIn[mod],
			FanOut: fanOut[mod],
			Score:  fanIn[mod] + fanOut[mod],
		}
		if s.Score > 0 {
			scored = append(scored, s)
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	// Show top 10
	limit := 10
	if len(scored) < limit {
		limit = len(scored)
	}

	if limit == 0 {
		sb.WriteString("_No cross-module dependencies detected._\n\n")
		return sb.String()
	}

	sb.WriteString("| Module | Fan-In | Fan-Out | Criticality |\n")
	sb.WriteString("|--------|--------|---------|-------------|\n")
	for _, s := range scored[:limit] {
		criticality := "low"
		if s.Score >= 10 {
			criticality = "high"
		} else if s.Score >= 5 {
			criticality = "medium"
		}
		sb.WriteString(fmt.Sprintf("| `%s` | %d | %d | %s |\n", s.Name, s.FanIn, s.FanOut, criticality))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderRiskZones(snapshot *facts.Snapshot) string {
	var risks []string

	for _, insight := range snapshot.Insights {
		if strings.Contains(insight.Title, "Cyclic dependency") ||
			strings.Contains(insight.Title, "Layer violation") {
			risks = append(risks, fmt.Sprintf("- **%s** (confidence: %.0f%%): %s",
				insight.Title, insight.Confidence*100, insight.Description))
		}
	}

	if len(risks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Risk Zones\n\n")
	for _, risk := range risks {
		sb.WriteString(risk + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderFeatureGuide(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("## How to Add a Feature\n\n")

	// Determine guide based on detected architecture
	var archPattern string
	for _, insight := range snapshot.Insights {
		if strings.HasPrefix(insight.Title, "Architecture pattern:") {
			archPattern = strings.TrimPrefix(insight.Title, "Architecture pattern: ")
			break
		}
	}

	// Detect dominant language for platform-specific guidance.
	dominantLang := detectDominantLanguage(snapshot)

	switch archPattern {
	case "hexagonal":
		if dominantLang == "swift" {
			sb.WriteString("This project follows a clean architecture pattern (iOS/Swift):\n\n")
			sb.WriteString("1. **Define domain model** in Domain/Models\n")
			sb.WriteString("2. **Define repository protocol** in Domain/Repositories\n")
			sb.WriteString("3. **Implement use case** in Domain/UseCases\n")
			sb.WriteString("4. **Implement repository** in Data/Repositories (calls API services)\n")
			sb.WriteString("5. **Create ViewModel** in Presentation/ (depends on use cases via DI)\n")
			sb.WriteString("6. **Build SwiftUI View** consuming the ViewModel\n")
			sb.WriteString("7. **Wire dependencies** in Core/DI/DIContainer\n")
		} else {
			sb.WriteString("This project follows a hexagonal/clean architecture:\n\n")
			sb.WriteString("1. **Define domain types** in the domain/model layer\n")
			sb.WriteString("2. **Define a port** (interface) in the port layer for external interactions\n")
			sb.WriteString("3. **Implement the use case** in the application/service layer\n")
			sb.WriteString("4. **Implement adapters** for infrastructure (DB, API clients, etc.)\n")
			sb.WriteString("5. **Add the handler** (HTTP/gRPC) in the handler layer\n")
			sb.WriteString("6. **Wire dependencies** in the main/cmd entry point\n")
		}

	case "nextjs":
		sb.WriteString("This project follows a Next.js architecture:\n\n")
		sb.WriteString("1. **Create the page/route** in the `app/` or `pages/` directory\n")
		sb.WriteString("2. **Build UI components** in `components/`\n")
		sb.WriteString("3. **Add hooks** for client-side logic in `hooks/`\n")
		sb.WriteString("4. **Add server-side logic** as API routes or server actions\n")
		sb.WriteString("5. **Add shared types** in `types/`\n")
		sb.WriteString("6. **Add utility functions** in `lib/` or `utils/`\n")

	case "go-standard":
		sb.WriteString("This project follows Go standard project layout:\n\n")
		sb.WriteString("1. **Add the command** in `cmd/` if it's a new binary\n")
		sb.WriteString("2. **Implement business logic** in `internal/`\n")
		sb.WriteString("3. **Add shared libraries** in `pkg/` (if intended for external use)\n")
		sb.WriteString("4. **Define API contracts** in `api/`\n")
		sb.WriteString("5. **Wire the feature** in the appropriate `cmd/` main file\n")

	default:
		sb.WriteString("General guidance:\n\n")
		sb.WriteString("1. Identify the appropriate module/package for the feature\n")
		sb.WriteString("2. Follow existing patterns in the codebase\n")
		sb.WriteString("3. Keep dependencies flowing in one direction\n")
		sb.WriteString("4. Add appropriate exports for cross-module usage\n")
		sb.WriteString("5. Wire the feature in the entry point\n")
	}

	sb.WriteString("\n")
	return sb.String()
}

func (r *LLMContextRenderer) renderMeta(snapshot *facts.Snapshot) string {
	var sb strings.Builder
	sb.WriteString("---\n\n")
	sb.WriteString(fmt.Sprintf("*Generated at %s in %s. %d facts, %d insights.*\n",
		snapshot.Meta.GeneratedAt, snapshot.Meta.Duration,
		snapshot.Meta.FactCount, snapshot.Meta.InsightCount))
	return sb.String()
}

// detectDominantLanguage returns the most common language across module facts.
func detectDominantLanguage(snapshot *facts.Snapshot) string {
	counts := make(map[string]int)
	for _, f := range snapshot.Facts {
		if f.Kind == facts.KindModule {
			if lang, ok := f.Props["language"].(string); ok {
				counts[lang]++
			}
		}
	}
	best := ""
	bestCount := 0
	for lang, count := range counts {
		if count > bestCount {
			best = lang
			bestCount = count
		}
	}
	return best
}

func propStr(f facts.Fact, key string) string {
	if f.Props == nil {
		return ""
	}
	s, _ := f.Props[key].(string)
	return s
}

// propInt reads an int-valued prop, tolerating the float64 form produced by a
// JSON (facts.jsonl) round-trip.
func propInt(f facts.Fact, key string) int {
	if f.Props == nil {
		return 0
	}
	switch v := f.Props[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// propStrSlice reads a string-slice prop, tolerating the []interface{} form
// produced by a JSON round-trip.
func propStrSlice(f facts.Fact, key string) []string {
	if f.Props == nil {
		return nil
	}
	switch v := f.Props[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func filterByKind(ff []facts.Fact, kind string) []facts.Fact {
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func fileDir(file string) string {
	parts := strings.Split(file, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}
