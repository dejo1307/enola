package crossrepo

import (
	"context"
	"strings"
	"testing"

	"github.com/dejo1307/enola/internal/facts"
)

func TestExplain_SummarizesCrossRepoEdges(t *testing.T) {
	s := facts.NewStore()
	s.Add(
		facts.Fact{
			Kind: facts.KindDependency, Name: "svc-alpha -> svc-beta", Repo: "svc-alpha",
			Props: map[string]any{
				"type": "cross_repo", "synthetic": "crossrepo",
				"via": []string{"http"}, "endpoint_count": 3,
			},
		},
		facts.Fact{
			Kind: facts.KindDependency, Name: "app-web-app -> app-web", Repo: "app-web-app",
			Props: map[string]any{
				"type": "cross_repo", "synthetic": "crossrepo",
				"via": []string{"import"}, "import_count": 7,
			},
		},
		// A non-cross-repo dependency must be ignored.
		facts.Fact{Kind: facts.KindDependency, Name: "a -> b", Repo: "svc-alpha"},
	)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights = %d, want 1", len(insights))
	}
	ins := insights[0]
	if !strings.Contains(ins.Title, "2 edges") {
		t.Errorf("title = %q, want it to mention 2 edges", ins.Title)
	}
	if len(ins.Evidence) != 2 {
		t.Errorf("evidence count = %d, want 2", len(ins.Evidence))
	}
	if !strings.Contains(ins.Description, "svc-alpha -> svc-beta") ||
		!strings.Contains(ins.Description, "3 endpoint") {
		t.Errorf("description missing expected detail: %q", ins.Description)
	}
}

func TestExplain_NoCrossRepoFactsReturnsNil(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "m"})
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if insights != nil {
		t.Errorf("insights = %+v, want nil", insights)
	}
}

// propInt tolerates the float64 form produced by a JSON round-trip.
func TestPropInt_JSONRoundTripForm(t *testing.T) {
	d := facts.Fact{Props: map[string]any{"endpoint_count": float64(5)}}
	if got := propInt(d, "endpoint_count"); got != 5 {
		t.Errorf("propInt(float64 5) = %d, want 5", got)
	}
}
