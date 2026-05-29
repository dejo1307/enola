package layers

import (
	"context"
	"math"
	"testing"

	"github.com/dejo1307/enola/internal/facts"
)

// --- helpers ---

func makeModules(names ...string) []facts.Fact {
	var ff []facts.Fact
	for _, n := range names {
		ff = append(ff, facts.Fact{Kind: facts.KindModule, Name: n})
	}
	return ff
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

// --- Unit tests ---

func TestMatchesLayer(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{"middle segment matches", "src/domain/user", []string{"domain"}, true},
		{"exact segment not substring", "src/domain_helper", []string{"domain"}, false},
		{"case insensitive", "Domain/UseCases", []string{"domain"}, true},
		{"no match", "src/foo/bar", []string{"domain"}, false},
		{"first segment matches", "cmd/server", []string{"cmd"}, true},
		{"last segment matches", "src/api", []string{"api"}, true},
		{"multiple patterns", "src/models", []string{"model", "models"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesLayer(tt.path, tt.patterns)
			if got != tt.want {
				t.Errorf("matchesLayer(%q, %v) = %v, want %v", tt.path, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestDetectPatterns_Hexagonal(t *testing.T) {
	modules := makeModules("domain/entity", "application/usecase", "adapter/rest", "presentation/views")
	e := New()
	patterns := e.detectPatterns(modules)

	var hexPattern *archPattern
	for _, p := range patterns {
		if p.Name == "hexagonal" {
			hexPattern = p
			break
		}
	}
	if hexPattern == nil {
		t.Fatal("expected hexagonal pattern to be detected")
	}

	// 4/4 modules classified, 4/7 layers matched
	// confidence = (4/4)*0.6 + (4/7)*0.4 ≈ 0.828
	expectedConf := (4.0/4.0)*0.6 + (4.0/7.0)*0.4
	if math.Abs(hexPattern.Confidence-expectedConf) > 0.01 {
		t.Errorf("confidence = %f, want ≈ %f", hexPattern.Confidence, expectedConf)
	}

	if len(hexPattern.Layers) != 4 {
		t.Errorf("matched layers = %d, want 4", len(hexPattern.Layers))
	}
}

func TestDetectPatterns_NextJS(t *testing.T) {
	modules := makeModules("app", "components", "hooks", "lib")
	e := New()
	patterns := e.detectPatterns(modules)

	var found bool
	for _, p := range patterns {
		if p.Name == "nextjs" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected nextjs pattern to be detected")
	}
}

func TestDetectPatterns_GoStandard(t *testing.T) {
	modules := makeModules("cmd/server", "internal/auth", "pkg/utils")
	e := New()
	patterns := e.detectPatterns(modules)

	var found bool
	for _, p := range patterns {
		if p.Name == "go-standard" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected go-standard pattern to be detected")
	}
}

func TestDetectPatterns_BelowThreshold(t *testing.T) {
	// Only 1 module matches 1 layer out of many unrelated modules
	modules := makeModules("domain", "foo", "bar", "baz", "qux", "quux", "corge", "grault", "garply", "waldo")
	e := New()
	patterns := e.detectPatterns(modules)

	// With 1/10 modules matching and 1/7 layers for hexagonal:
	// confidence = (1/10)*0.6 + (1/7)*0.4 = 0.06 + 0.057 = 0.117 < 0.2 threshold
	for _, p := range patterns {
		if p.Name == "hexagonal" {
			t.Errorf("hexagonal should not be detected with confidence %f (below 0.2 threshold)", p.Confidence)
		}
	}
}

func TestDetectViolations_InnerImportsOuter(t *testing.T) {
	store := makeStore(
		[]string{"domain/entity", "presentation/views"},
		map[string][]string{
			"domain/entity": {"presentation/views"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// Should find at least one layer violation
	hasViolation := false
	for _, insight := range insights {
		if insight.Title != "" && insight.Confidence == 0.8 {
			hasViolation = true
			break
		}
	}
	if !hasViolation {
		t.Error("expected a layer violation insight when domain imports presentation")
	}
}

func TestDetectViolations_OuterImportsInner(t *testing.T) {
	store := makeStore(
		[]string{"domain/entity", "presentation/views"},
		map[string][]string{
			"presentation/views": {"domain/entity"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// No violation — outer importing inner is correct
	for _, insight := range insights {
		if insight.Confidence == 0.8 {
			t.Errorf("unexpected layer violation: %s", insight.Title)
		}
	}
}

func TestBestPattern(t *testing.T) {
	e := New()

	patterns := []*archPattern{
		{Name: "a", Confidence: 0.5},
		{Name: "b", Confidence: 0.8},
		{Name: "c", Confidence: 0.3},
	}

	best := e.bestPattern(patterns)
	if best.Name != "b" {
		t.Errorf("bestPattern = %s, want b (highest confidence)", best.Name)
	}
}

func TestBestPattern_Empty(t *testing.T) {
	e := New()
	if got := e.bestPattern(nil); got != nil {
		t.Errorf("bestPattern(nil) = %v, want nil", got)
	}
}

func TestExplain_NoModules(t *testing.T) {
	store := facts.NewStore()
	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for empty store, got %d", len(insights))
	}
}
