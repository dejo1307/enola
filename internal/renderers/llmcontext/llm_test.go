package llmcontext

import (
	"context"
	"strings"
	"testing"

	"github.com/dejo1307/enola/internal/facts"
)

func makeSnapshot(ff []facts.Fact, insights []facts.Insight) *facts.Snapshot {
	return &facts.Snapshot{
		Meta: facts.SnapshotMeta{
			GeneratedAt:  "2024-01-01T00:00:00Z",
			Duration:     "1s",
			FactCount:    len(ff),
			InsightCount: len(insights),
		},
		Facts:    ff,
		Insights: insights,
	}
}

func TestTokenBudgetEnforcement(t *testing.T) {
	// Create a snapshot with enough facts to generate long content
	var ff []facts.Fact
	for i := 0; i < 50; i++ {
		ff = append(ff, facts.Fact{
			Kind: facts.KindModule,
			Name: strings.Repeat("module_", 10) + string(rune('A'+i%26)),
			Props: map[string]any{
				"language": "go",
			},
		})
	}

	snapshot := makeSnapshot(ff, nil)

	// Small token budget (100 tokens = 400 chars) — enough for truncation logic
	// to work (maxChars-100 must be positive; see BUG note below)
	r := New(100)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	content := string(artifacts[0].Content)
	hasTruncation := strings.Contains(content, "[Truncated in:") || strings.Contains(content, "[Omitted:")
	if !hasTruncation {
		t.Error("expected truncation or omission marker in output")
	}
	// Content should be within budget (400 chars + truncation/omission message)
	maxExpected := 100*4 + 80
	if len(content) > maxExpected {
		t.Errorf("content length %d exceeds expected truncated size %d", len(content), maxExpected)
	}
}

func TestDetectDominantLanguage(t *testing.T) {
	tests := []struct {
		name     string
		facts    []facts.Fact
		wantLang string
	}{
		{
			"go dominant",
			[]facts.Fact{
				{Kind: facts.KindModule, Props: map[string]any{"language": "go"}},
				{Kind: facts.KindModule, Props: map[string]any{"language": "go"}},
				{Kind: facts.KindModule, Props: map[string]any{"language": "go"}},
				{Kind: facts.KindModule, Props: map[string]any{"language": "typescript"}},
			},
			"go",
		},
		{
			"no modules",
			nil,
			"",
		},
		{
			"single language",
			[]facts.Fact{
				{Kind: facts.KindModule, Props: map[string]any{"language": "swift"}},
			},
			"swift",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := makeSnapshot(tt.facts, nil)
			got := detectDominantLanguage(snapshot)
			if got != tt.wantLang {
				t.Errorf("detectDominantLanguage = %q, want %q", got, tt.wantLang)
			}
		})
	}
}

func TestRender_EmptySnapshot(t *testing.T) {
	snapshot := makeSnapshot(nil, nil)
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	content := string(artifacts[0].Content)
	if !strings.Contains(content, "# Architecture Snapshot") {
		t.Error("expected Architecture Snapshot header")
	}
	if !strings.Contains(content, "_No modules detected._") {
		t.Error("expected 'No modules detected' fallback")
	}
	if !strings.Contains(content, "_No entry points detected._") {
		t.Error("expected 'No entry points detected' fallback")
	}
}

func TestRiskZones_IncludesCyclesAndViolations(t *testing.T) {
	insights := []facts.Insight{
		{Title: "Architecture pattern: hexagonal", Confidence: 0.8, Description: "Detected hexagonal"},
		{Title: "Cyclic dependency detected (3 modules)", Confidence: 1.0, Description: "A -> B -> C -> A"},
		{Title: "Layer violation: domain -> presentation", Confidence: 0.8, Description: "Domain imports presentation"},
	}

	snapshot := makeSnapshot(nil, insights)
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	content := string(artifacts[0].Content)

	// Cycle and violation should appear in Risk Zones
	if !strings.Contains(content, "Cyclic dependency") {
		t.Error("expected Cyclic dependency in Risk Zones")
	}
	if !strings.Contains(content, "Layer violation") {
		t.Error("expected Layer violation in Risk Zones")
	}

	// Architecture pattern should NOT appear in Risk Zones section
	// It appears in the Architecture Pattern section instead
	riskIdx := strings.Index(content, "## Risk Zones")
	if riskIdx >= 0 {
		riskSection := content[riskIdx:]
		nextSection := strings.Index(riskSection[1:], "## ")
		if nextSection > 0 {
			riskSection = riskSection[:nextSection+1]
		}
		if strings.Contains(riskSection, "Architecture pattern") {
			t.Error("Architecture pattern insight should NOT appear in Risk Zones")
		}
	}
}

func TestCriticalModules_FanInFanOut(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindModule, Name: "core"},
		{Kind: facts.KindModule, Name: "a"},
		{Kind: facts.KindModule, Name: "b"},
		{Kind: facts.KindModule, Name: "c"},
	}
	// a, b, c all import core → core has fanIn=3
	for _, src := range []string{"a", "b", "c"} {
		ff = append(ff, facts.Fact{
			Kind: facts.KindDependency,
			File: src + "/file.go",
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: "core"},
			},
		})
	}

	snapshot := makeSnapshot(ff, nil)
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	content := string(artifacts[0].Content)
	if !strings.Contains(content, "`core`") {
		t.Error("expected core module in Critical Modules table")
	}
	// core has fanIn=3, fanOut=0, score=3 → "low" criticality
	if !strings.Contains(content, "| 3 | 0 |") {
		t.Error("expected core to have fanIn=3, fanOut=0")
	}
}

func TestFileDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"src/foo/bar.go", "src/foo"},
		{"file.go", "."},
		{"a/b/c/d.go", "a/b/c"},
	}
	for _, tt := range tests {
		got := fileDir(tt.input)
		if got != tt.want {
			t.Errorf("fileDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
