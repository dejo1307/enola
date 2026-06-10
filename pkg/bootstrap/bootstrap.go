package bootstrap

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	crossrepoexp "github.com/enola-labs/enola/internal/explainers/crossrepo"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/extractors/kotlinextractor"
	"github.com/enola-labs/enola/internal/extractors/openapiextractor"
	"github.com/enola-labs/enola/internal/extractors/pythonextractor"
	"github.com/enola-labs/enola/internal/extractors/rubyextractor"
	"github.com/enola-labs/enola/internal/extractors/swiftextractor"
	"github.com/enola-labs/enola/internal/extractors/tsextractor"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/renderers/llmcontext"
	"github.com/enola-labs/enola/internal/server"
	"github.com/enola-labs/enola/pkg/plugin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine wraps the internal engine with a public interface for
// extension by enterprise or third-party code.
type Engine struct {
	eng *engine.Engine
}

// Store returns the underlying fact store.
func (e *Engine) Store() *facts.Store {
	return e.eng.Store()
}

// Snapshot returns the last generated snapshot, or nil.
func (e *Engine) Snapshot() *facts.Snapshot {
	return e.eng.Snapshot()
}

// SetSnapshot sets the snapshot (used when auto-loading from disk).
func (e *Engine) SetSnapshot(snap *facts.Snapshot) {
	e.eng.SetSnapshot(snap)
}

// ResolveFactFile returns the absolute filesystem path for a fact's File field.
func (e *Engine) ResolveFactFile(f *facts.Fact) string {
	return e.eng.ResolveFactFile(f)
}

// RepoPaths returns the repo label -> absolute path mapping.
func (e *Engine) RepoPaths() map[string]string {
	return e.eng.RepoPaths()
}

// Config returns the engine config.
func (e *Engine) Config() *config.Config {
	return e.eng.Config()
}

// GenerateSnapshot runs the full pipeline: walk -> extract -> explain -> render.
func (e *Engine) GenerateSnapshot(ctx context.Context, repoPath string, appendMode bool) (*facts.Snapshot, error) {
	return e.eng.GenerateSnapshot(ctx, repoPath, appendMode)
}

// WriteArtifacts writes all snapshot artifacts to the output directory.
func (e *Engine) WriteArtifacts(repoPath string) error {
	return e.eng.WriteArtifacts(repoPath)
}

// GetArtifact returns the content of a named artifact.
func (e *Engine) GetArtifact(name string) ([]byte, error) {
	return e.eng.GetArtifact(name)
}

// RegisterExtractor adds an extractor to the engine.
func (e *Engine) RegisterExtractor(ext plugin.Extractor) {
	e.eng.RegisterExtractor(ext)
}

// RegisterExplainer adds an explainer to the engine.
func (e *Engine) RegisterExplainer(exp plugin.Explainer) {
	e.eng.RegisterExplainer(exp)
}

// RegisterRenderer adds a renderer to the engine.
func (e *Engine) RegisterRenderer(rnd plugin.Renderer) {
	e.eng.RegisterRenderer(rnd)
}

// Server wraps the MCP server with a public interface.
type Server struct {
	srv *server.Server
}

// Run starts the MCP server on the stdio transport.
func (s *Server) Run(ctx context.Context) error {
	return s.srv.Run(ctx)
}

// SetToolCallback sets a callback invoked each time a tool is called.
func (s *Server) SetToolCallback(cb func(string)) {
	s.srv.SetToolCallback(cb)
}

// StartTime returns the time the server started (zero value if Run() hasn't been called).
func (s *Server) StartTime() time.Time {
	return s.srv.GetStartTime()
}

// MCP returns the underlying MCP server so enterprise code can register
// additional (license-gated) tools before calling Run.
func (s *Server) MCP() *mcp.Server {
	return s.srv.MCPServer()
}

// Options controls bootstrap behavior.
type Options struct {
	// ConfigPath is the path to the YAML config file. Default: "mcp-arch.yaml".
	ConfigPath string
}

// NewEngine creates an Engine with all OSS plugins registered.
// Use the returned Engine's methods to add additional (enterprise) plugins
// before starting the server or generating snapshots.
func NewEngine(opts Options) (*Engine, *config.Config, error) {
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = "mcp-arch.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil && !filepath.IsAbs(cfgPath) {
		if exePath, exErr := os.Executable(); exErr == nil {
			exeDir := filepath.Dir(exePath)
			cfg, err = config.Load(filepath.Join(exeDir, cfgPath))
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v, using defaults\n", err)
		cfg = config.Default()
	}

	eng, err := engine.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create engine: %w", err)
	}

	// Register all OSS extractors
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExtractor(kotlinextractor.New())
	eng.RegisterExtractor(openapiextractor.New())
	eng.RegisterExtractor(pythonextractor.New())
	eng.RegisterExtractor(tsextractor.New())
	eng.RegisterExtractor(swiftextractor.New())
	eng.RegisterExtractor(rubyextractor.New())

	// Register all OSS explainers
	eng.RegisterExplainer(cycles.New())
	eng.RegisterExplainer(layers.New())
	eng.RegisterExplainer(crossrepoexp.New())

	// Register all OSS renderers
	eng.RegisterRenderer(llmcontext.New(cfg.Output.MaxContextTokens))

	return &Engine{eng: eng}, cfg, nil
}

// NewServer creates an MCP server wired to the given Engine.
func NewServer(eng *Engine, cfg *config.Config) (*Server, error) {
	srv, err := server.New(eng.eng, cfg)
	if err != nil {
		return nil, err
	}
	return &Server{srv: srv}, nil
}

// AutoLoadSnapshot loads an existing snapshot from disk if available.
// This allows queries to work immediately without a generate_snapshot call.
func AutoLoadSnapshot(eng *Engine, cfg *config.Config) {
	repoPath, err := filepath.Abs(cfg.Repo)
	if err != nil {
		return
	}

	factsPath := filepath.Join(repoPath, cfg.Output.Dir, "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		return
	}

	log.Printf("[bootstrap] loading existing snapshot from %s", factsPath)
	if err := eng.Store().ReadJSONLFile(factsPath); err != nil {
		log.Printf("[bootstrap] warning: failed to load existing facts: %v", err)
		return
	}

	repoLabel := filepath.Base(repoPath)
	eng.Store().SetRepoRange(0, repoLabel)
	eng.Store().BuildGraph()
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: repoPath},
	})
	log.Printf("[bootstrap] loaded %d facts from existing snapshot", eng.Store().Count())
}
