package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/dejo1307/enola/internal/config"
	"github.com/dejo1307/enola/internal/engine"
	"github.com/dejo1307/enola/internal/explainers/cycles"
	"github.com/dejo1307/enola/internal/explainers/layers"
	"github.com/dejo1307/enola/internal/extractors/goextractor"
	"github.com/dejo1307/enola/internal/extractors/kotlinextractor"
	"github.com/dejo1307/enola/internal/extractors/openapiextractor"
	"github.com/dejo1307/enola/internal/extractors/pythonextractor"
	"github.com/dejo1307/enola/internal/extractors/rubyextractor"
	"github.com/dejo1307/enola/internal/extractors/swiftextractor"
	"github.com/dejo1307/enola/internal/extractors/tsextractor"
	"github.com/dejo1307/enola/internal/facts"
	"github.com/dejo1307/enola/internal/renderers/llmcontext"
	"github.com/dejo1307/enola/internal/server"
)

func main() {
	// Ensure log output goes to stderr, never stdout (MCP uses stdout for JSON-RPC)
	log.SetOutput(os.Stderr)

	ctx := context.Background()

	generateMode := false
	cfgPath := "mcp-arch.yaml"
	for _, arg := range os.Args[1:] {
		if arg == "--generate" {
			generateMode = true
		} else {
			cfgPath = arg
		}
	}

	// If the config path is relative, resolve it first against the current
	// working directory, then (as a fallback) against the directory containing
	// the binary itself. This ensures the config is found when Cursor starts
	// the MCP server from a different working directory.
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
		log.Fatalf("failed to create engine: %v", err)
	}

	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExtractor(kotlinextractor.New())
	eng.RegisterExtractor(openapiextractor.New())
	eng.RegisterExtractor(pythonextractor.New())
	eng.RegisterExtractor(tsextractor.New())
	eng.RegisterExtractor(swiftextractor.New())
	eng.RegisterExtractor(rubyextractor.New())

	eng.RegisterExplainer(cycles.New())
	eng.RegisterExplainer(layers.New())

	eng.RegisterRenderer(llmcontext.New(cfg.Output.MaxContextTokens))

	if generateMode {
		repoPath, err := filepath.Abs(cfg.Repo)
		if err != nil {
			log.Fatalf("failed to resolve repo path: %v", err)
		}

		snapshot, err := eng.GenerateSnapshot(ctx, repoPath, false)
		if err != nil {
			log.Fatalf("snapshot generation failed: %v", err)
		}

		if err := eng.WriteArtifacts(repoPath); err != nil {
			log.Fatalf("failed to write artifacts: %v", err)
		}

		fmt.Fprintf(os.Stderr, "\nSnapshot complete:\n")
		fmt.Fprintf(os.Stderr, "  Repository:  %s\n", snapshot.Meta.RepoPath)
		fmt.Fprintf(os.Stderr, "  Facts:       %d\n", snapshot.Meta.FactCount)
		fmt.Fprintf(os.Stderr, "  Insights:    %d\n", snapshot.Meta.InsightCount)
		fmt.Fprintf(os.Stderr, "  Artifacts:   %d\n", len(snapshot.Artifacts))
		fmt.Fprintf(os.Stderr, "  Duration:    %s\n", snapshot.Meta.Duration)
		fmt.Fprintf(os.Stderr, "  Output:      %s\n", filepath.Join(repoPath, cfg.Output.Dir))
		os.Exit(0)
	}

	// Auto-load existing snapshot if available (so queries work immediately
	// without requiring a generate_snapshot call first).
	if repoPath, err := filepath.Abs(cfg.Repo); err == nil {
		factsPath := filepath.Join(repoPath, cfg.Output.Dir, "facts.jsonl")
		if _, err := os.Stat(factsPath); err == nil {
			log.Printf("[main] loading existing snapshot from %s", factsPath)
			if err := eng.Store().ReadJSONLFile(factsPath); err != nil {
				log.Printf("[main] warning: failed to load existing facts: %v", err)
			} else {
				repoLabel := filepath.Base(repoPath)
				eng.Store().SetRepoRange(0, repoLabel)
				eng.Store().BuildGraph()
				eng.SetSnapshot(&facts.Snapshot{
					Meta: facts.SnapshotMeta{RepoPath: repoPath},
				})
				log.Printf("[main] loaded %d facts from existing snapshot", eng.Store().Count())
			}
		}
	}

	srv, err := server.New(eng, cfg)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
