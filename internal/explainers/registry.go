package explainers

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// Explainer analyzes facts and produces architectural insights.
type Explainer interface {
	// Name returns the explainer identifier (e.g. "cycles", "layers").
	Name() string
	// Explain analyzes the fact store and returns insights.
	Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error)
}

// Registry holds registered explainers.
type Registry struct {
	explainers []Explainer
}

// NewRegistry creates a new explainer registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an explainer to the registry.
func (r *Registry) Register(e Explainer) {
	r.explainers = append(r.explainers, e)
}

// Get returns the explainer with the given name, or nil if not found.
func (r *Registry) Get(name string) Explainer {
	for _, e := range r.explainers {
		if e.Name() == name {
			return e
		}
	}
	return nil
}

// All returns all registered explainers.
func (r *Registry) All() []Explainer {
	return r.explainers
}
