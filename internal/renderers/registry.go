package renderers

import (
	"context"

	"github.com/dejo1307/enola/internal/facts"
)

// Renderer produces output artifacts from a snapshot.
type Renderer interface {
	// Name returns the renderer identifier (e.g. "llm_context").
	Name() string
	// Render produces artifacts from the given snapshot.
	Render(ctx context.Context, snapshot *facts.Snapshot) ([]facts.Artifact, error)
}

// Registry holds registered renderers.
type Registry struct {
	renderers []Renderer
}

// NewRegistry creates a new renderer registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a renderer to the registry.
func (r *Registry) Register(rnd Renderer) {
	r.renderers = append(r.renderers, rnd)
}

// Get returns the renderer with the given name, or nil if not found.
func (r *Registry) Get(name string) Renderer {
	for _, rnd := range r.renderers {
		if rnd.Name() == name {
			return rnd
		}
	}
	return nil
}

// All returns all registered renderers.
func (r *Registry) All() []Renderer {
	return r.renderers
}
