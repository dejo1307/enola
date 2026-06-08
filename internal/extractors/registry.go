package extractors

import (
	"github.com/enola-labs/enola/pkg/plugin"
)

// Extractor parses source files for a specific language and emits architectural facts.
// Deprecated: use plugin.Extractor instead. This alias is kept for backward compatibility.
type Extractor = plugin.Extractor

// Registry holds registered extractors.
type Registry struct {
	extractors []Extractor
}

// NewRegistry creates a new extractor registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an extractor to the registry.
func (r *Registry) Register(e Extractor) {
	r.extractors = append(r.extractors, e)
}

// Get returns the extractor with the given name, or nil if not found.
func (r *Registry) Get(name string) Extractor {
	for _, e := range r.extractors {
		if e.Name() == name {
			return e
		}
	}
	return nil
}

// All returns all registered extractors.
func (r *Registry) All() []Extractor {
	return r.extractors
}

// DetectAll returns extractors that support the given repository.
func (r *Registry) DetectAll(repoPath string) ([]Extractor, error) {
	var matched []Extractor
	for _, e := range r.extractors {
		ok, err := e.Detect(repoPath)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, e)
		}
	}
	return matched, nil
}
