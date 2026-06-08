package explainers

import (
	"github.com/enola-labs/enola/pkg/plugin"
)

// Explainer analyzes facts and produces architectural insights.
// Deprecated: use plugin.Explainer instead. This alias is kept for backward compatibility.
type Explainer = plugin.Explainer

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
