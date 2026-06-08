package plugin

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

// Explainer analyzes facts and produces architectural insights.
type Explainer interface {
	// Name returns the explainer identifier (e.g. "cycles", "layers").
	Name() string
	// Explain analyzes the fact store and returns insights.
	Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error)
}

// Renderer produces output artifacts from a snapshot.
type Renderer interface {
	// Name returns the renderer identifier (e.g. "llm_context").
	Name() string
	// Render produces artifacts from the given snapshot.
	Render(ctx context.Context, snapshot *facts.Snapshot) ([]facts.Artifact, error)
}
