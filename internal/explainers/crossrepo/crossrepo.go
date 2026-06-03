// Package crossrepo provides an explainer that summarizes the cross-repo
// dependency edges synthesized by internal/linkers/crossrepo into
// human-readable insights for the LLM context and insights.json output.
package crossrepo

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dejo1307/enola/internal/facts"
)

// CrossRepoExplainer turns synthetic cross_repo dependency facts into insights.
type CrossRepoExplainer struct{}

// New creates a new CrossRepoExplainer.
func New() *CrossRepoExplainer {
	return &CrossRepoExplainer{}
}

func (e *CrossRepoExplainer) Name() string {
	return "crossrepo"
}

// Explain reads the cross_repo dependency facts the linker already added to the
// store and emits one summarizing insight describing how the repositories
// depend on each other. It returns nothing for single-repo snapshots.
func (e *CrossRepoExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	deps, _ := store.QueryAdvanced(facts.QueryOpts{
		Kind:      facts.KindDependency,
		Prop:      "type",
		PropValue: "cross_repo",
		Limit:     500,
	})
	if len(deps) == 0 {
		return nil, nil
	}

	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })

	evidence := make([]facts.Evidence, 0, len(deps))
	var lines []string
	for _, d := range deps {
		detail := edgeDetail(d)
		evidence = append(evidence, facts.Evidence{
			Fact:   d.Name,
			Detail: detail,
		})
		lines = append(lines, d.Name+" ("+detail+")")
	}

	insight := facts.Insight{
		Title: fmt.Sprintf("Cross-repo dependencies (%d edges)", len(deps)),
		Description: "These service-to-service dependencies span repositories: " +
			strings.Join(lines, "; ") + ". Traverse from a repo label (service node) " +
			"to follow requests across repos.",
		Confidence: 0.9,
		Evidence:   evidence,
	}
	return []facts.Insight{insight}, nil
}

// edgeDetail renders a short description of what justifies a cross-repo edge.
func edgeDetail(d facts.Fact) string {
	var parts []string
	if v, ok := d.Props["via"].([]string); ok && len(v) > 0 {
		parts = append(parts, "via "+strings.Join(v, "+"))
	}
	if n := propInt(d, "endpoint_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d endpoint(s)", n))
	}
	if n := propInt(d, "import_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d import(s)", n))
	}
	if len(parts) == 0 {
		return "cross-repo dependency"
	}
	return strings.Join(parts, ", ")
}

// propInt reads an int-valued prop, tolerating the float64 form that survives a
// JSON round-trip through facts.jsonl.
func propInt(d facts.Fact, key string) int {
	if d.Props == nil {
		return 0
	}
	switch v := d.Props[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}
