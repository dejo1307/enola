package renderers

import (
	"github.com/enola-labs/enola/pkg/plugin"
)

// Renderer produces output artifacts from a snapshot.
// Deprecated: use plugin.Renderer instead. This alias is kept for backward compatibility.
type Renderer = plugin.Renderer

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
