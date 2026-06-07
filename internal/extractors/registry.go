package extractors

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// Extractor parses source files for a specific language and emits architectural facts.
type Extractor interface {
	// Name returns the extractor identifier (e.g. "go", "typescript").
	Name() string
	// Detect returns true if this extractor supports the given repository.
	Detect(repoPath string) (bool, error)
	// Extract parses files in the repository and returns extracted facts.
	Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error)
}

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
