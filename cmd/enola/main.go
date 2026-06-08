package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

func main() {
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

	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: cfgPath,
	})
	if err != nil {
		log.Fatalf("failed to create engine: %v", err)
	}

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

	bootstrap.AutoLoadSnapshot(eng, cfg)

	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
